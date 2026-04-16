package envoy_mcp_openapi_processor

import (
	"strconv"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
)

func httpStatusResponse(status typev3.StatusCode) *extProcPb.ProcessingResponse {
	return &extProcPb.ProcessingResponse{
		Response: &extProcPb.ProcessingResponse_ImmediateResponse{
			ImmediateResponse: &extProcPb.ImmediateResponse{
				Status: &typev3.HttpStatus{
					Code: status,
				},
			},
		},
	}
}

func appendHeader(headers []*corev3.HeaderValueOption, key string, value string) []*corev3.HeaderValueOption {
	return append(headers, &corev3.HeaderValueOption{
		Header: &corev3.HeaderValue{
			Key:      key,
			RawValue: []byte(value),
		},
		AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
	})
}

func rerouteWithBodyMutation(host string, method string, path string, body []byte, extraHeaders map[string]string) *extProcPb.ProcessingResponse {
	count := 4 + len(extraHeaders) // (method, path, authority, content-length) + extra headers
	headers := make([]*corev3.HeaderValueOption, 0, count)

	for key, value := range extraHeaders {
		headers = appendHeader(headers, key, value)
	}

	headers = appendHeader(headers, ":method", method)
	headers = appendHeader(headers, ":path", path)
	headers = appendHeader(headers, ":authority", host)
	headers = appendHeader(headers, "content-length", strconv.Itoa(len(body)))

	return &extProcPb.ProcessingResponse{
		Response: &extProcPb.ProcessingResponse_RequestBody{
			RequestBody: &extProcPb.BodyResponse{
				Response: &extProcPb.CommonResponse{
					HeaderMutation: &extProcPb.HeaderMutation{
						SetHeaders: headers,
					},
					BodyMutation: &extProcPb.BodyMutation{
						Mutation: &extProcPb.BodyMutation_Body{
							Body: body,
						},
					},
				},
			},
		},
	}
}

func newImmediateBodyResponse(body []byte) *extProcPb.ProcessingResponse {
	return &extProcPb.ProcessingResponse{
		Response: &extProcPb.ProcessingResponse_ImmediateResponse{
			ImmediateResponse: &extProcPb.ImmediateResponse{
				Body: body,
				Status: &typev3.HttpStatus{
					Code: typev3.StatusCode_OK,
				},
				Headers: &extProcPb.HeaderMutation{
					SetHeaders: []*corev3.HeaderValueOption{
						{
							Header: &corev3.HeaderValue{
								Key:      "content-type",
								RawValue: []byte("application/json; charset=utf-8"),
							},
							AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
						},
					},
				},
			},
		},
	}
}
