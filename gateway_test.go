// Copyright (c) 2022 Cloudflare, Inc. All rights reserved.
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/chris-wood/ohttp-go"
	"github.com/cloudflare/circl/hpke"
	"google.golang.org/protobuf/proto"
)

var (
	LEGACY_KEY_ID    = uint8(0x00)
	CURRENT_KEY_ID   = uint8(LEGACY_KEY_ID + 1)
	FORBIDDEN_TARGET = "forbidden.example"
	ALLOWED_TARGET   = "allowed.example"
	GATEWAY_DEBUG    = true
)

func createGateway(t *testing.T) ohttp.Gateway {
	legacyConfig, err := ohttp.NewConfig(LEGACY_KEY_ID, hpke.KEM_X25519_HKDF_SHA256, hpke.KDF_HKDF_SHA256, hpke.AEAD_AES128GCM)
	if err != nil {
		t.Fatal("Failed to create a valid config. Exiting now.")
	}
	config, err := ohttp.NewConfig(CURRENT_KEY_ID, hpke.KEM_X25519_KYBER768_DRAFT00, hpke.KDF_HKDF_SHA256, hpke.AEAD_AES128GCM)
	if err != nil {
		t.Fatal("Failed to create a valid config. Exiting now.")
	}

	return ohttp.NewDefaultGateway([]ohttp.PrivateConfig{config, legacyConfig})
}

type MockMetrics struct {
	eventName    string
	resultLabels map[string]bool
}

func (s *MockMetrics) ResponseStatus(prefix string, status int) {
	s.Fire(fmt.Sprintf("%s_response_status_%d", prefix, status))
}

func (s *MockMetrics) Fire(result string) {
	// This just assertion that we don't call the `Fire` twice for the same result/label
	_, exists := s.resultLabels[result]
	if exists {
		panic("Metrics.Fire called twice for the same result")
	}
	s.resultLabels[result] = true
}

type MockMetricsFactory struct {
	metrics []*MockMetrics
}

func (f *MockMetricsFactory) Create(eventName string) Metrics {
	metrics := &MockMetrics{
		eventName:    eventName,
		resultLabels: map[string]bool{},
	}
	f.metrics = append(f.metrics, metrics)
	return metrics
}

type ForbiddenCheckHttpRequestHandler struct {
	forbidden string
}

func mustGetMetricsFactory(t *testing.T, gateway gatewayResource) *MockMetricsFactory {
	factory, ok := gateway.metricsFactory.(*MockMetricsFactory)
	if !ok {
		panic("Failed to get metrics factory")
	}
	return factory
}

func (h ForbiddenCheckHttpRequestHandler) Handle(req *http.Request, metrics Metrics) (*http.Response, error) {
	if req.Host == h.forbidden {
		metrics.Fire(metricsResultTargetRequestForbidden)
		return nil, ErrGatewayTargetForbidden
	}

	metrics.Fire(metricsResultSuccess)
	return &http.Response{
		StatusCode: http.StatusOK,
	}, nil
}

func createMockEchoGatewayServer(t *testing.T) gatewayResource {
	gateway := createGateway(t)
	echoEncapHandler := DefaultEncapsulationHandler{
		gateway:    gateway,
		appHandler: EchoAppHandler{},
	}
	mockProtoHTTPFilterHandler := DefaultEncapsulationHandler{
		gateway: gateway,
		appHandler: ProtoHTTPAppHandler{
			httpHandler: ForbiddenCheckHttpRequestHandler{
				FORBIDDEN_TARGET,
			},
		},
	}

	encapHandlers := make(map[string]EncapsulationHandler)
	encapHandlers[defaultEchoEndpoint] = echoEncapHandler
	encapHandlers[defaultGatewayEndpoint] = mockProtoHTTPFilterHandler
	return gatewayResource{
		gateway:               gateway,
		encapsulationHandlers: encapHandlers,
		debugResponse:         GATEWAY_DEBUG,
		metricsFactory:        &MockMetricsFactory{},
	}
}

func TestLegacyConfigHandler(t *testing.T) {
	target := createMockEchoGatewayServer(t)
	config, err := target.gateway.Config(LEGACY_KEY_ID)
	if err != nil {
		t.Fatal(err)
	}
	marshalledConfig := config.Marshal()

	handler := http.HandlerFunc(target.legacyConfigHandler)

	request, err := http.NewRequest("GET", defaultLegacyConfigEndpoint, nil)
	if err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, request)

	if status := rr.Code; status != http.StatusOK {
		t.Fatal(fmt.Errorf("Failed request with error code: %d", status))
	}

	body, err := ioutil.ReadAll(rr.Result().Body)
	if err != nil {
		t.Fatal("Failed to read body:", err)
	}

	if !bytes.Equal(body, marshalledConfig) {
		t.Fatal("Received invalid config")
	}

	// checking correct header exists
	// Cache-Control: max-age=%d, private
	cctrl := rr.Header().Get("Cache-Control")

	if strings.HasPrefix(cctrl, "max-age=") && strings.HasSuffix(cctrl, ", private") {
		maxAge := strings.TrimPrefix(strings.TrimSuffix(cctrl, ", private"), "max-age=")
		age, err := strconv.Atoi(maxAge)
		if err != nil {
			t.Fatal("max-age value should be int", err)
		}
		if age < twelveHours || age > twelveHours+twentyFourHours {
			t.Fatal("age should be between 12 and 36 hours")
		}
	} else {
		t.Fatal("Cache-Control format should be 'max-age=86400, private'")
	}
}

func TestConfigHandler(t *testing.T) {
	target := createMockEchoGatewayServer(t)
	marshalledConfigs := target.gateway.MarshalConfigs()

	handler := http.HandlerFunc(target.configHandler)
	request, err := http.NewRequest("GET", defaultConfigEndpoint, nil)
	if err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, request)

	if status := rr.Code; status != http.StatusOK {
		t.Fatal(fmt.Errorf("Failed request with error code: %d", status))
	}

	body, err := ioutil.ReadAll(rr.Result().Body)
	if err != nil {
		t.Fatal("Failed to read body:", err)
	}

	if !bytes.Equal(body, marshalledConfigs) {
		t.Fatal("Received invalid config")
	}

	// checking correct header exists
	// Cache-Control: max-age=%d, private
	cctrl := rr.Header().Get("Cache-Control")

	if strings.HasPrefix(cctrl, "max-age=") && strings.HasSuffix(cctrl, ", private") {
		maxAge := strings.TrimPrefix(strings.TrimSuffix(cctrl, ", private"), "max-age=")
		age, err := strconv.Atoi(maxAge)
		if err != nil {
			t.Fatal("max-age value should be int", err)
		}
		if age < twelveHours || age > twelveHours+twentyFourHours {
			t.Fatal("age should be between 12 and 36 hours")
		}
	} else {
		t.Fatal("Cache-Control format should be 'max-age=86400, private'")
	}
}

func testBodyContainsError(t *testing.T, resp *http.Response, expectedText string) {
	body, err := io.ReadAll(resp.Body)
	if err == nil {
		if !strings.Contains(string(body), expectedText) {
			t.Fatal(fmt.Errorf("Failed to return expected text (%s) in response. Body text is: %s",
				expectedText, body))
		}
	}
}

func testMetricsContainsResult(t *testing.T, metricsCollector *MockMetricsFactory, event, result string) {
	for _, metric := range metricsCollector.metrics {
		if metric.eventName == event {
			_, exists := metric.resultLabels[result]
			if !exists {
				t.Fatalf("Expected event %s/%s was not fired. resultLabels len=%d", event, result, len(metric.resultLabels))
			} else {
				return
			}
		}
	}
	t.Fatalf("Expected metric for event %s was not initialized", event)
}

func TestQueryHandlerInvalidContentType(t *testing.T) {
	target := createMockEchoGatewayServer(t)

	handler := http.HandlerFunc(target.gatewayHandler)

	request, err := http.NewRequest(http.MethodPost, defaultGatewayEndpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Add("Content-Type", "application/not-the-droids-youre-looking-for")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, request)

	if status := rr.Result().StatusCode; status != http.StatusBadRequest {
		t.Fatal(fmt.Errorf("Result did not yield %d, got %d instead", http.StatusBadRequest, status))
	}

	testBodyContainsError(t, rr.Result(), "Invalid content type: application/not-the-droids-youre-looking-for")
	testMetricsContainsResult(t, mustGetMetricsFactory(t, target), metricsEventGatewayRequest, metricsResultInvalidContentType)
}

func TestLegacyGatewayHandler(t *testing.T) {
	target := createMockEchoGatewayServer(t)

	handler := http.HandlerFunc(target.gatewayHandler)

	config, err := target.gateway.Config(LEGACY_KEY_ID)
	if err != nil {
		t.Fatal(err)
	}
	client := ohttp.NewDefaultClient(config)

	testMessage := []byte{0xCA, 0xFE}
	req, _, err := client.EncapsulateRequest(testMessage)
	if err != nil {
		t.Fatal(err)
	}

	request, err := http.NewRequest(http.MethodPost, defaultEchoEndpoint, bytes.NewReader(req.Marshal()))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Add("Content-Type", "message/ohttp-req")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, request)

	if status := rr.Result().StatusCode; status != http.StatusOK {
		t.Fatal(fmt.Errorf("Result did not yield %d, got %d instead", http.StatusOK, status))
	}
	if rr.Result().Header.Get("Content-Type") != "message/ohttp-res" {
		t.Fatal("Invalid content type response")
	}

	testMetricsContainsResult(t, mustGetMetricsFactory(t, target), metricsEventGatewayRequest, metricsResultSuccess)
}

func TestGatewayHandler(t *testing.T) {
	target := createMockEchoGatewayServer(t)

	handler := http.HandlerFunc(target.gatewayHandler)

	config, err := target.gateway.Config(CURRENT_KEY_ID)
	if err != nil {
		t.Fatal(err)
	}
	client := ohttp.NewDefaultClient(config)

	testMessage := []byte{0xCA, 0xFE}
	req, _, err := client.EncapsulateRequest(testMessage)
	if err != nil {
		t.Fatal(err)
	}

	request, err := http.NewRequest(http.MethodPost, defaultEchoEndpoint, bytes.NewReader(req.Marshal()))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Add("Content-Type", "message/ohttp-req")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, request)

	if status := rr.Result().StatusCode; status != http.StatusOK {
		t.Fatal(fmt.Errorf("Result did not yield %d, got %d instead", http.StatusOK, status))
	}
	if rr.Result().Header.Get("Content-Type") != "message/ohttp-res" {
		t.Fatal("Invalid content type response")
	}

	testMetricsContainsResult(t, mustGetMetricsFactory(t, target), metricsEventGatewayRequest, metricsResultSuccess)
}

func TestGatewayHandlerWithInvalidMethod(t *testing.T) {
	target := createMockEchoGatewayServer(t)

	handler := http.HandlerFunc(target.gatewayHandler)

	config, err := target.gateway.Config(CURRENT_KEY_ID)
	if err != nil {
		t.Fatal(err)
	}
	client := ohttp.NewDefaultClient(config)

	testMessage := []byte{0xCA, 0xFE}
	req, _, err := client.EncapsulateRequest(testMessage)
	if err != nil {
		t.Fatal(err)
	}

	request, err := http.NewRequest(http.MethodGet, defaultEchoEndpoint, bytes.NewReader(req.Marshal()))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Add("Content-Type", "message/ohttp-req")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, request)

	if status := rr.Result().StatusCode; status != http.StatusBadRequest {
		t.Fatal(fmt.Errorf("Result did not yield %d, got %d instead", http.StatusBadRequest, status))
	}

	testMetricsContainsResult(t, mustGetMetricsFactory(t, target), metricsEventGatewayRequest, metricsResultInvalidMethod)
}

func TestGatewayHandlerWithInvalidKey(t *testing.T) {
	target := createMockEchoGatewayServer(t)

	handler := http.HandlerFunc(target.gatewayHandler)

	// Generate a new config that's different from the target's
	privateConfig, err := ohttp.NewConfig(CURRENT_KEY_ID, hpke.KEM_X25519_HKDF_SHA256, hpke.KDF_HKDF_SHA256, hpke.AEAD_AES128GCM)
	if err != nil {
		t.Fatal("Failed to create a valid config. Exiting now.")
	}
	client := ohttp.NewDefaultClient(privateConfig.Config())

	testMessage := []byte{0xCA, 0xFE}
	req, _, err := client.EncapsulateRequest(testMessage)
	if err != nil {
		t.Fatal(err)
	}

	request, err := http.NewRequest(http.MethodPost, defaultEchoEndpoint, bytes.NewReader(req.Marshal()))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Add("Content-Type", "message/ohttp-req")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, request)

	if status := rr.Result().StatusCode; status != http.StatusBadRequest {
		t.Fatal(fmt.Errorf("Result did not yield %d, got %d instead", http.StatusBadRequest, status))
	}

	testMetricsContainsResult(t, mustGetMetricsFactory(t, target), metricsEventGatewayRequest, metricsResultDecapsulationFailed)
}

func TestGatewayHandlerWithUnknownKey(t *testing.T) {
	target := createMockEchoGatewayServer(t)

	handler := http.HandlerFunc(target.gatewayHandler)

	// Generate a new config that's different from the target's in the key ID
	privateConfig, err := ohttp.NewConfig(CURRENT_KEY_ID^0xFF, hpke.KEM_X25519_HKDF_SHA256, hpke.KDF_HKDF_SHA256, hpke.AEAD_AES128GCM)
	if err != nil {
		t.Fatal("Failed to create a valid config. Exiting now.")
	}
	client := ohttp.NewDefaultClient(privateConfig.Config())

	testMessage := []byte{0xCA, 0xFE}
	req, _, err := client.EncapsulateRequest(testMessage)
	if err != nil {
		t.Fatal(err)
	}

	request, err := http.NewRequest(http.MethodPost, defaultEchoEndpoint, bytes.NewReader(req.Marshal()))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Add("Content-Type", "message/ohttp-req")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, request)

	if status := rr.Result().StatusCode; status != http.StatusUnauthorized {
		t.Fatal(fmt.Errorf("Result did not yield %d, got %d instead", http.StatusUnauthorized, status))
	}

	testMetricsContainsResult(t, mustGetMetricsFactory(t, target), metricsEventGatewayRequest, metricsResultConfigurationMismatch)
}

func TestGatewayHandlerWithCorruptContent(t *testing.T) {
	target := createMockEchoGatewayServer(t)

	handler := http.HandlerFunc(target.gatewayHandler)

	config, err := target.gateway.Config(CURRENT_KEY_ID)
	if err != nil {
		t.Fatal(err)
	}
	client := ohttp.NewDefaultClient(config)

	// Corrupt the message
	testMessage := []byte{0xCA, 0xFE}
	req, _, err := client.EncapsulateRequest(testMessage)
	if err != nil {
		t.Fatal(err)
	}

	reqEnc := req.Marshal()
	reqEnc[len(reqEnc)-1] ^= 0xFF

	request, err := http.NewRequest(http.MethodPost, defaultEchoEndpoint, bytes.NewReader(reqEnc))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Add("Content-Type", "message/ohttp-req")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, request)

	if status := rr.Result().StatusCode; status != http.StatusBadRequest {
		t.Fatal(fmt.Errorf("Result did not yield %d, got %d instead", http.StatusBadRequest, status))
	}

	testMetricsContainsResult(t, mustGetMetricsFactory(t, target), metricsEventGatewayRequest, metricsResultDecapsulationFailed)
}

func TestGatewayHandlerProtoHTTPRequestWithForbiddenTarget(t *testing.T) {
	target := createMockEchoGatewayServer(t)

	handler := http.HandlerFunc(target.gatewayHandler)

	config, err := target.gateway.Config(CURRENT_KEY_ID)
	if err != nil {
		t.Fatal(err)
	}
	client := ohttp.NewDefaultClient(config)

	httpRequest, err := http.NewRequest(http.MethodPost, fmt.Sprintf("http://%s%s", FORBIDDEN_TARGET, defaultGatewayEndpoint), nil)
	if err != nil {
		t.Fatal(err)
	}

	binaryRequest, err := requestToProtoHTTP(httpRequest)
	if err != nil {
		t.Fatal(err)
	}

	encodedRequest, err := proto.Marshal(binaryRequest)
	if err != nil {
		t.Fatal(err)
	}
	req, context, err := client.EncapsulateRequest(encodedRequest)
	if err != nil {
		t.Fatal(err)
	}

	reqEnc := req.Marshal()

	request, err := http.NewRequest(http.MethodPost, defaultGatewayEndpoint, bytes.NewReader(reqEnc))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Add("Content-Type", "message/ohttp-req")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, request)

	if status := rr.Result().StatusCode; status != http.StatusOK {
		t.Fatal(fmt.Errorf("Result did not yield %d, got %d instead", http.StatusOK, status))
	}

	bodyBytes, err := ioutil.ReadAll(rr.Body)
	if err != nil {
		t.Fatal(err)
	}

	encapResp, err := ohttp.UnmarshalEncapsulatedResponse(bodyBytes)
	if err != nil {
		t.Fatal(err)
	}

	binaryResp, err := context.DecapsulateResponse(encapResp)
	if err != nil {
		t.Fatal(err)
	}

	resp := &Response{}
	if err := proto.Unmarshal(binaryResp, resp); err != nil {
		t.Fatal(err)
	}

	if resp.StatusCode != http.StatusForbidden {
		t.Fatal(fmt.Errorf("Encapsulated result did not yield %d, got %d instead", http.StatusForbidden, resp.StatusCode))
	}

	testMetricsContainsResult(t, mustGetMetricsFactory(t, target), metricsEventGatewayRequest, metricsResultTargetRequestForbidden)
}

func TestGatewayHandlerProtoHTTPRequestWithAllowedTarget(t *testing.T) {
	target := createMockEchoGatewayServer(t)

	handler := http.HandlerFunc(target.gatewayHandler)

	config, err := target.gateway.Config(CURRENT_KEY_ID)
	if err != nil {
		t.Fatal(err)
	}
	client := ohttp.NewDefaultClient(config)

	httpRequest, err := http.NewRequest(http.MethodPost, fmt.Sprintf("http://%s%s", ALLOWED_TARGET, defaultGatewayEndpoint), nil)
	if err != nil {
		t.Fatal(err)
	}

	binaryRequest, err := requestToProtoHTTP(httpRequest)
	if err != nil {
		t.Fatal(err)
	}

	encodedRequest, err := proto.Marshal(binaryRequest)
	if err != nil {
		t.Fatal(err)
	}
	req, context, err := client.EncapsulateRequest(encodedRequest)
	if err != nil {
		t.Fatal(err)
	}

	reqEnc := req.Marshal()

	request, err := http.NewRequest(http.MethodPost, defaultGatewayEndpoint, bytes.NewReader(reqEnc))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Add("Content-Type", "message/ohttp-req")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, request)

	if status := rr.Result().StatusCode; status != http.StatusOK {
		t.Fatal(fmt.Errorf("Result did not yield %d, got %d instead", http.StatusOK, status))
	}

	if status := rr.Result().StatusCode; status != http.StatusOK {
		t.Fatal(fmt.Errorf("Result did not yield %d, got %d instead", http.StatusOK, status))
	}

	bodyBytes, err := ioutil.ReadAll(rr.Body)
	if err != nil {
		t.Fatal(err)
	}

	encapResp, err := ohttp.UnmarshalEncapsulatedResponse(bodyBytes)
	if err != nil {
		t.Fatal(err)
	}

	binaryResp, err := context.DecapsulateResponse(encapResp)
	if err != nil {
		t.Fatal(err)
	}

	resp := &Response{}
	if err := proto.Unmarshal(binaryResp, resp); err != nil {
		t.Fatal(err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Fatal(fmt.Errorf("Encapsulated result did not yield %d, got %d instead", http.StatusOK, resp.StatusCode))
	}

	testMetricsContainsResult(t, mustGetMetricsFactory(t, target), metricsEventGatewayRequest, metricsResultSuccess)
}
