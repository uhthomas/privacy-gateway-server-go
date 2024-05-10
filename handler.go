// Copyright (c) 2022 Cloudflare, Inc. All rights reserved.
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httputil"

	"github.com/chris-wood/ohttp-go"
)

// Description of the error handling in the specification:
// https://ietf-wg-ohai.github.io/oblivious-http/draft-ietf-ohai-ohttp.html#name-errors:

// 401 - Unauthorized in Gateway response
var ErrConfigMismatch = errors.New("configuration mismatch")

// 400 - BadRequest in Gateway response
var ErrEncapsulation = errors.New("encapsulation error")

// 400 - BadRequest in Payload response. Payload is not a valid protobuf or marshalling error.
var ErrPayloadMarshalling = errors.New("issues with payload marshalling (BHTTP or Protobuf)")

// 403 - Forbidden in Payload response. The request is not allowed to be sent to the target.
var ErrGatewayTargetForbidden = errors.New("target forbidden on gateway (request was blocked by gateway)")

// 500 - Internal server error in Payload response. The request failed to be processed after decapsulation.
var ErrGatewayInternalServer = errors.New("the request failed to be processed after decapsulation")

// Errors happened during decapsulation/encapsulation are returned as gateway response's error status (401 and 400)
func ErrEncapsulationToGatewayStatusCode(e error) int {
	switch e {
	case ErrConfigMismatch:
		return http.StatusUnauthorized
	case ErrEncapsulation:
		return http.StatusBadRequest
	default:
		return http.StatusBadRequest
	}
}

// Errors happened after decapsulation are returned as encapsulated payload errors while gatewy status is 200
func payloadErrorToPayloadStatusCode(e error) int {
	switch e {
	case ErrPayloadMarshalling:
		return http.StatusBadRequest
	case ErrGatewayTargetForbidden:
		return http.StatusForbidden
	case ErrGatewayInternalServer:
		return http.StatusInternalServerError
	default:
		return 400
	}
}

// EncapsulationFail is called when the gateway is unable to decapsulate the request or unable to encapsulate the response. Leads to 401 or 400 on gateway level
func EncapsulationFail(err error) (ohttp.EncapsulatedResponse, error) {
	return ohttp.EncapsulatedResponse{}, err
}

const (
	// Metrics constants
	metricsResultConfigurationMismatch     = "config_mismatch"
	metricsResultDecapsulationFailed       = "decapsulation_failed"
	metricsResultEncapsulationFailed       = "encapsulation_failed"
	metricsResultContentDecodingFailed     = "content_decode_failed"
	metricsResultContentEncodingFailed     = "content_encode_failed"
	metricsResultRequestTranslationFailed  = "request_translate_failed"
	metricsResultResponseTranslationFailed = "response_translate_failed"
	metricsResultTargetRequestForbidden    = "request_forbidden"
	metricsResultTargetRequestFailed       = "request_failed"
	metricsResultSuccess                   = "success"
	metricsPayloadStatusPrefix             = "gateway_payload"
)

// EncapsulationHandler handles OHTTP encapsulated requests and produces OHTTP encapsulated responses.
type EncapsulationHandler interface {
	// Handle processes an OHTTP encapsulated request and produces an OHTTP encapsulated response, or an error
	// if any part of the encapsulation or decapsulation process fails.
	Handle(outerRequest *http.Request, encapRequest ohttp.EncapsulatedRequest, metrics Metrics) (ohttp.EncapsulatedResponse, error)
}

// DefaultEncapsulationHandler is an EncapsulationHandler that uses a default OHTTP gateway to decapsulate
// requests, pass them to an AppContentHandler to produce a response for encapsulation, and encapsulates the
// response.
type DefaultEncapsulationHandler struct {
	gateway    ohttp.Gateway
	appHandler AppContentHandler
}

// Handle attempts to decapsulate the incoming encapsulated request and, if successful, passes the
// corresponding application payload to the AppContentHandler for producing a response to encapsulate
// and return.
func (h DefaultEncapsulationHandler) Handle(outerRequest *http.Request, encapsulatedReq ohttp.EncapsulatedRequest, metrics Metrics) (ohttp.EncapsulatedResponse, error) {
	if !h.gateway.MatchesConfig(encapsulatedReq) {
		metrics.Fire(metricsResultConfigurationMismatch)
		return EncapsulationFail(ErrConfigMismatch)
	}

	binaryRequest, context, err := h.gateway.DecapsulateRequest(encapsulatedReq)
	if err != nil {
		metrics.Fire(metricsResultDecapsulationFailed)
		return EncapsulationFail(ErrEncapsulation)
	}

	e := NewEncapsulatedChunkWriter(w)

	if err := h.appHandler.Handle(e, binaryRequest, metrics); err != nil {
		return EncapsulationFail(err)
	}

	encapsulatedResponse, err := context.EncapsulateResponse(binaryResponse)
	if err != nil {
		metrics.Fire(metricsResultEncapsulationFailed)
		return EncapsulationFail(ErrEncapsulation)
	}

	return encapsulatedResponse, nil
}

// MetadataEncapsulationHandler is an EncapsulationHandler that uses a default OHTTP gateway to decapsulate
// requests and return metadata about the encapsulated request context as an encapsulated response. Metadata
// includes, for example, the list of headers carried on the encapsulated request from the client or relay.
type MetadataEncapsulationHandler struct {
	gateway ohttp.Gateway
}

// Handle attempts to decapsulate the incoming encapsulated request and, if successful, foramts
// metadata from the request context, and then encapsulates and returns the result.
func (h MetadataEncapsulationHandler) Handle(outerRequest *http.Request, encapsulatedReq ohttp.EncapsulatedRequest, metrics Metrics) (ohttp.EncapsulatedResponse, error) {
	if !h.gateway.MatchesConfig(encapsulatedReq) {
		metrics.Fire(metricsResultConfigurationMismatch)
		return EncapsulationFail(ErrConfigMismatch)
	}

	_, context, err := h.gateway.DecapsulateRequest(encapsulatedReq)
	if err != nil {
		metrics.Fire(metricsResultDecapsulationFailed)
		return EncapsulationFail(ErrEncapsulation)
	}

	// XXX(caw): maybe also include the encapsulated request and its plaintext form too?
	binaryResponse, err := httputil.DumpRequest(outerRequest, false)
	if err != nil {
		// Note: we don't record an event for this as it's not necessary to track
		return EncapsulationFail(ErrGatewayInternalServer)
	}

	encapsulatedResponse, err := context.EncapsulateResponse(binaryResponse)
	if err != nil {
		metrics.Fire(metricsResultEncapsulationFailed)
		return EncapsulationFail(ErrEncapsulation)
	}

	metrics.Fire(metricsResultSuccess)
	return encapsulatedResponse, nil
}

// AppContentHandler processes application-specific request content and produces response content.
type AppContentHandler interface {
	Handle(e *EncapsulatedChunkWriter, binaryRequest []byte, metrics Metrics) error
}

// // EchoAppHandler is an AppContentHandler that returns the application request as the response.
// type EchoAppHandler struct{}

// // Handle returns the input request as the response.
// func (h EchoAppHandler) Handle(binaryRequest []byte, metrics Metrics) ([]byte, error) {
// 	metrics.Fire(metricsResultSuccess)
// 	return binaryRequest, nil
// }

// // ProtoHTTPAppHandler is an AppContentHandler that parses the application request as
// // a protobuf-based HTTP request for resolution with an HttpRequestHandler.
// type ProtoHTTPAppHandler struct {
// 	httpHandler HttpRequestHandler
// }

// // returns the same object format as for PayloadSuccess moving error inside successful response
// func (h ProtoHTTPAppHandler) wrappedError(e error, metrics Metrics) ([]byte, error) {
// 	status := payloadErrorToPayloadStatusCode(e)
// 	resp := &Response{
// 		StatusCode: int32(status),
// 		Body:       []byte(e.Error()),
// 	}
// 	respEnc, err := proto.Marshal(resp)
// 	if err != nil {
// 		return nil, err
// 	}
// 	metrics.ResponseStatus(metricsPayloadStatusPrefix, status)
// 	return respEnc, nil
// }

// // Handle attempts to parse the application payload as a protobuf-based HTTP request and, if successful,
// // translates the result into an equivalent http.Request object to be processed by the handler's HttpRequestHandler.
// // The http.Response result from the handler is then translated back into an equivalent protobuf-based HTTP
// // response and returned to the caller.
// func (h ProtoHTTPAppHandler) Handle(binaryRequest []byte, metrics Metrics) ([]byte, error) {
// 	req := &Request{}
// 	if err := proto.Unmarshal(binaryRequest, req); err != nil {
// 		metrics.Fire(metricsResultContentDecodingFailed)
// 		return h.wrappedError(ErrPayloadMarshalling, metrics)
// 	}

// 	httpRequest, err := protoHTTPToRequest(req)
// 	if err != nil {
// 		metrics.Fire(metricsResultRequestTranslationFailed)
// 		return h.wrappedError(ErrPayloadMarshalling, metrics)
// 	}

// 	httpResponse, err := h.httpHandler.Handle(httpRequest, metrics)
// 	if err != nil {
// 		if err == ErrGatewayTargetForbidden {
// 			// Return 403 (Forbidden) in the event the client request was for a
// 			// Target not on the allow list
// 			return h.wrappedError(ErrGatewayTargetForbidden, metrics)
// 		}
// 		return h.wrappedError(ErrGatewayInternalServer, metrics)
// 	}

// 	protoResponse, err := responseToProtoHTTP(httpResponse)
// 	if err != nil {
// 		metrics.Fire(metricsResultResponseTranslationFailed)
// 		return h.wrappedError(ErrPayloadMarshalling, metrics)
// 	}

// 	marshalledProtoResponse, err := proto.Marshal(protoResponse)
// 	if err != nil {
// 		metrics.Fire(metricsResultContentEncodingFailed)
// 		return h.wrappedError(ErrPayloadMarshalling, metrics)
// 	}
// 	metrics.Fire(metricsPayloadStatusPrefix + "200")
// 	return marshalledProtoResponse, nil
// }

// BinaryHTTPAppHandler is an AppContentHandler that parses the application request as
// a binary HTTP request for resolution with an HttpRequestHandler.
type BinaryHTTPAppHandler struct {
	httpHandler HttpRequestHandler
}

// func (h BinaryHTTPAppHandler) wrappedError(e error, metrics Metrics) ([]byte, error) {
// 	status := payloadErrorToPayloadStatusCode(e)
// 	resp := &http.Response{
// 		StatusCode: status,
// 		Body:       io.NopCloser(bytes.NewBufferString(e.Error())),
// 	}
// 	binaryResponse := ohttp.CreateBinaryResponse(resp)
// 	metrics.Fire(metricsPayloadStatusPrefix + strconv.Itoa(status))
// 	return binaryResponse.Marshal()
// }

// Handle attempts to parse the application payload as a binary HTTP request and, if successful,
// translates the result into an equivalent http.Request object to be processed by the handler's HttpRequestHandler.
// The http.Response result from the handler is then translated back into an equivalent binary HTTP
// response and returned to the caller.
func (h BinaryHTTPAppHandler) Handle(e *EncapsulatedChunkWriter, binaryRequest []byte, metrics Metrics) error {
	req, err := ohttp.UnmarshalBinaryRequest(binaryRequest)
	if err != nil {
		metrics.Fire(metricsResultContentDecodingFailed)
		// return h.wrappedError(ErrPayloadMarshalling, metrics)
		return ErrPayloadMarshalling
	}

	resp, err := h.httpHandler.Handle(req, metrics)
	if err != nil {
		if err == ErrGatewayTargetForbidden {
			// Return 403 (Forbidden) in the event the client request was for a
			// Target not on the allow list
			// return h.wrappedError(ErrGatewayTargetForbidden,
			// metrics)
			return ErrGatewayTargetForbidden
		}
		// return h.wrappedError(ErrGatewayInternalServer, metrics)
		return ErrGatewayInternalServer
	}

	if err := NewBinaryResponseEncoder(e).Encode(resp); err != nil {
		metrics.Fire(metricsResultContentEncodingFailed)
		// return h.wrappedError(ErrPayloadMarshalling, metrics)
		return ErrPayloadMarshalling
	}

	metrics.Fire(metricsPayloadStatusPrefix + "200")
	return nil
}

// HttpRequestHandler handles HTTP requests to produce responses.
type HttpRequestHandler interface {
	// Handle takes a http.Request and resolves it to produce a http.Response.
	Handle(req *http.Request, metrics Metrics) (*http.Response, error)
}

// FilteredHttpRequestHandler represents a HttpRequestHandler that restricts
// outbound HTTP requests to an allowed set of targets.
type FilteredHttpRequestHandler struct {
	client             *http.Client
	allowedOrigins     map[string]bool
	logForbiddenErrors bool
}

// Handle processes HTTP requests to targets that are permitted according to a list of
// allowed targets.
func (h FilteredHttpRequestHandler) Handle(req *http.Request, metrics Metrics) (*http.Response, error) {
	if h.allowedOrigins != nil {
		_, ok := h.allowedOrigins[req.Host]
		if !ok {
			metrics.Fire(metricsResultTargetRequestForbidden)
			if h.logForbiddenErrors {
				// to allow clients to fix improper third party urls usage (e.g. to change URLs from our direct s3 refs to CDN)
				log.Printf("TargetForbiddenError: %s, %s", req.Host, req.URL)
			}
			return nil, ErrGatewayTargetForbidden
		}
	}

	resp, err := h.client.Do(req)
	if err != nil {
		metrics.Fire(metricsResultTargetRequestFailed)
		return nil, err
	}

	metrics.Fire(metricsResultSuccess)
	return resp, nil
}

type EncapsulatedChunkWriter struct {
	w io.Writer
}

func NewEncapsulatedChunkWriter(w io.Writer) *EncapsulatedChunkWriter {
	return &EncapsulatedChunkWriter{
		w: w,
	}
}

func (e *EncapsulatedChunkWriter) Write(b []byte) (int, error) {
	return e.w.Write(b)
}

func (e *EncapsulatedChunkWriter) Close() error {
	return nil
}

type BinaryResponseEncoder struct {
	w         io.Writer
	ChunkSize int
}

func NewBinaryResponseEncoder(w io.Writer) *BinaryResponseEncoder {
	return &BinaryResponseEncoder{
		w:         w,
		ChunkSize: 32 << 10,
	}
}

func (e *BinaryResponseEncoder) Encode(res *http.Response) error {
	bres := ohttp.CreateBinaryResponse(res)
	return bres.Write(e.w)
}
