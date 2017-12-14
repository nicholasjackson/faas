// Copyright (c) Alex Ellis 2017. All rights reserved.
// Licensed under the MIT license. See LICENSE file in the project root for full license information.

package handlers

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"strconv"
	"time"

	"os"

	"github.com/Sirupsen/logrus"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"github.com/gorilla/mux"
	"github.com/openfaas/faas/gateway/metrics"
	"github.com/openfaas/faas/gateway/requests"
)

const watchdogPort = 8080

// MakeProxy creates a proxy for HTTP web requests which can be routed to a function.
func MakeProxy(metrics metrics.Metrics, wildcard bool, client *client.Client, logger *logrus.Logger) http.HandlerFunc {
	proxyClient := http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   3 * time.Second,
				KeepAlive: 0,
			}).DialContext,
			MaxIdleConns:          1,
			DisableKeepAlives:     true,
			IdleConnTimeout:       120 * time.Millisecond,
			ExpectContinueTimeout: 1500 * time.Millisecond,
		},
	}

	return func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()

		switch r.Method {
		case "POST", "GET":
			logger.Infoln(r.Header)

			xFunctionHeader := r.Header["X-Function"]
			if len(xFunctionHeader) > 0 {
				logger.Debugln(xFunctionHeader)
			}

			// getServiceName
			var serviceName string
			if wildcard {
				vars := mux.Vars(r)
				name := vars["name"]
				serviceName = name
			} else if len(xFunctionHeader) > 0 {
				serviceName = xFunctionHeader[0]
			}

			if len(serviceName) > 0 {
				lookupInvoke(w, r, metrics, serviceName, client, logger, &proxyClient)
			} else {
				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte("Provide an x-function header or valid route /function/function_name."))
			}
			break
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}
}

func lookupInvoke(w http.ResponseWriter, r *http.Request, metrics metrics.Metrics, name string, c *client.Client, logger *logrus.Logger, proxyClient *http.Client) {
	exists, err := lookupSwarmService(name, c)

	if err != nil || exists == false {
		if err != nil {
			logger.Errorf("Could not resolve service: %s error: %s.", name, err)
		}

		// TODO: Should record the 404/not found error in Prometheus.
		writeHead(name, metrics, http.StatusNotFound, w)
		w.Write([]byte(fmt.Sprintf("Cannot find service: %s.", name)))
	}

	if exists {
		defer trackTime(time.Now(), logger, metrics, name)
		forwardReq := requests.NewForwardRequest(r.Method, *r.URL)

		invokeService(w, r, metrics, name, forwardReq, logger, proxyClient)
	}
}

func lookupSwarmService(serviceName string, c *client.Client) (bool, error) {
	fmt.Printf("Resolving: '%s'\n", serviceName)
	serviceFilter := filters.NewArgs()
	serviceFilter.Add("name", serviceName)
	services, err := c.ServiceList(context.Background(), types.ServiceListOptions{Filters: serviceFilter})

	return len(services) > 0, err
}

func invokeService(w http.ResponseWriter, r *http.Request, metrics metrics.Metrics, service string, forwardReq requests.ForwardRequest, logger *logrus.Logger, proxyClient *http.Client) {
	stamp := strconv.FormatInt(time.Now().Unix(), 10)

	defer func(when time.Time) {
		trackTime(when, logger, metrics, service)
	}(time.Now())

	//TODO: inject setting rather than looking up each time.
	var dnsrr bool
	if os.Getenv("dnsrr") == "true" {
		dnsrr = true
	}

	addr := service
	// Use DNS-RR via tasks.servicename if enabled as override, otherwise VIP.
	if dnsrr {
		entries, lookupErr := net.LookupIP(fmt.Sprintf("tasks.%s", service))
		if lookupErr == nil && len(entries) > 0 {
			index := randomInt(0, len(entries))
			addr = entries[index].String()
		}
	}

	url := forwardReq.ToURL(addr, watchdogPort)

	contentType := r.Header.Get("Content-Type")
	logger.Infof("[%s] Forwarding request [%s] to: %s\n", stamp, contentType, url)

	if r.Body != nil {
		defer r.Body.Close()
	}

	request, err := http.NewRequest(r.Method, url, r.Body)

	copyHeaders(&request.Header, &r.Header)

	response, err := proxyClient.Do(request)
	if err != nil {
		logger.Errorln(err)

		writeHead(service, metrics, http.StatusInternalServerError, w)
		buf := bytes.NewBufferString("Can't reach service: " + service)
		w.Write(buf.Bytes())
		return
	}

	clientHeader := w.Header()
	copyHeaders(&clientHeader, &response.Header)

	defaultHeader := "text/plain"

	w.Header().Set("Content-Type", GetContentType(response.Header, r.Header, defaultHeader))

	writeHead(service, metrics, response.StatusCode, w)

	if response.Body != nil {
		io.Copy(w, response.Body)
	}
}

// GetContentType resolves the correct Content-Tyoe for a proxied function
func GetContentType(request http.Header, proxyResponse http.Header, defaultValue string) string {
	responseHeader := proxyResponse.Get("Content-Type")
	requestHeader := request.Get("Content-Type")

	var headerContentType string
	if len(responseHeader) > 0 {
		headerContentType = responseHeader
	} else if len(requestHeader) > 0 {
		headerContentType = requestHeader
	} else {
		headerContentType = defaultValue
	}

	return headerContentType
}

func copyHeaders(destination *http.Header, source *http.Header) {
	for k, v := range *source {
		vClone := make([]string, len(v))
		copy(vClone, v)
		(*destination)[k] = vClone
	}
}

func randomInt(min, max int) int {
	rand.Seed(time.Now().Unix())
	return rand.Intn(max-min) + min
}

func writeHead(service string, metrics metrics.Metrics, code int, w http.ResponseWriter) {
	w.WriteHeader(code)

	trackInvocation(service, metrics, code)
}

func trackInvocation(service string, metrics metrics.Metrics, code int) {
	metrics.GatewayFunctionInvocation(map[string]string{
		"function_name": service,
		"code":          strconv.Itoa(code),
	})
}

func trackTime(then time.Time, logger *logrus.Logger, metrics metrics.Metrics, name string) {
	since := time.Since(then)
	metrics.GatewayFunctionsHistogram(map[string]string{
		"function_name": name,
	},
		since,
	)

	logger.Infof("[%s] took %f seconds\n", name, since.Seconds())
}

func trackTimeExact(duration time.Duration, logger *logrus.Logger, metrics metrics.Metrics, name string) {
	metrics.GatewayFunctionsHistogram(map[string]string{
		"function_name": name,
	},
		duration,
	)

	logger.Infof("[%s] took %f seconds\n", name, duration.Seconds())
}
