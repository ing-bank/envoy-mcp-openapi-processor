package envoy_mcp_openapi_processor

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
)

type paramError struct {
	paramIn   string
	paramName string
	reason    string
}

type endpointRequest struct {
	path         string
	queryParams  url.Values
	extraHeaders map[string]string
}

func newEndpointRequest(endpoint endpoint, arguments map[string]any) (*endpointRequest, *paramError) {
	extraHeaders := map[string]string{
		// Overwrite the accept header from mcp client to support response content-types other than application/json
		"accept": "*/*",
	}
	if endpoint.ContentType != "" {
		extraHeaders["content-type"] = endpoint.ContentType
	}

	r := &endpointRequest{
		path:         endpoint.PathTemplate,
		queryParams:  url.Values{},
		extraHeaders: extraHeaders,
	}

	for paramName, paramConfig := range endpoint.Parameters {
		paramMap, ok := paramConfig.(map[string]any)
		if !ok {
			continue
		}

		paramIn, _ := paramMap["in"].(string)
		required, _ := paramMap["required"].(bool)
		value, exists := arguments[paramName]
		if !exists && required {
			return nil, &paramError{paramIn: paramIn, paramName: paramName, reason: "required but missing"}
		}
		if !exists {
			continue
		}

		if err := r.applyEndpointArgument(paramIn, paramName, value); err != nil {
			return nil, err
		}
	}

	return r, nil
}

func (r *endpointRequest) fullPath() string {
	if len(r.queryParams) > 0 {
		return fmt.Sprintf("%s?%s", r.path, r.queryParams.Encode())
	}
	return r.path
}

func jsonScalarToString(value any) (string, bool) {
	switch value.(type) {
	case string, float64, bool:
		return fmt.Sprintf("%v", value), true
	default:
		return "", false
	}
}

func scalarParamString(paramIn string, paramName string, value any) (string, *paramError) {
	str, ok := jsonScalarToString(value)
	if !ok {
		return "", &paramError{paramIn: paramIn, paramName: paramName, reason: "must be a scalar value"}
	}
	return str, nil
}

func queryParamValues(paramName string, value any) ([]string, *paramError) {
	if slice, ok := value.([]any); ok {
		values := make([]string, 0, len(slice))
		for _, elem := range slice {
			str, ok := jsonScalarToString(elem)
			if !ok {
				return nil, &paramError{paramIn: openapi3.ParameterInQuery, paramName: paramName, reason: "array elements must be scalar values"}
			}
			values = append(values, str)
		}
		return values, nil
	}

	str, ok := jsonScalarToString(value)
	if !ok {
		return nil, &paramError{paramIn: openapi3.ParameterInQuery, paramName: paramName, reason: "must be a scalar value or array of scalars"}
	}
	return []string{str}, nil
}

func (r *endpointRequest) applyEndpointArgument(paramIn string, paramName string, value any) *paramError {
	switch paramIn {
	case openapi3.ParameterInPath:
		str, err := scalarParamString(paramIn, paramName, value)
		if err != nil {
			return err
		}
		placeholder := fmt.Sprintf("{%s}", paramName)
		r.path = strings.ReplaceAll(r.path, placeholder, str)
	case openapi3.ParameterInQuery:
		values, err := queryParamValues(paramName, value)
		if err != nil {
			return err
		}
		for _, queryValue := range values {
			r.queryParams.Add(paramName, queryValue)
		}
	case openapi3.ParameterInHeader:
		str, err := scalarParamString(paramIn, paramName, value)
		if err != nil {
			return err
		}
		r.extraHeaders[paramName] = str
	}
	return nil
}

func marshalRequestBody(endpoint endpoint, arguments map[string]any) ([]byte, error) {
	if !endpoint.supportsBody() {
		return []byte{}, nil
	}
	bodyValue, exists := arguments["body"]
	if !exists {
		return []byte{}, nil
	}
	return json.Marshal(bodyValue)
}
