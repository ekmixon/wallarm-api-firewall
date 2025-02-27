package handlers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/savsgio/gotils/strconv"
	"github.com/sirupsen/logrus"
	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/fasthttpadaptor"
	"github.com/valyala/fastjson"
	"github.com/wallarm/api-firewall/internal/config"
	"github.com/wallarm/api-firewall/internal/platform/oauth2"
	"github.com/wallarm/api-firewall/internal/platform/proxy"
	"github.com/wallarm/api-firewall/internal/platform/shadowAPI"
	"github.com/wallarm/api-firewall/internal/platform/validator"
	"github.com/wallarm/api-firewall/internal/platform/web"
)

type openapiWaf struct {
	route           *routers.Route
	proxyPool       proxy.Pool
	logger          *logrus.Logger
	cfg             *config.APIFWConfiguration
	pathParamLength int
	parserPool      *fastjson.ParserPool
	oauthValidator  oauth2.OAuth2
	shadowAPI       shadowAPI.Checker
}

// EXPERIMENTAL feature
// returns APIFW-Validation-Status header value
func getValidationHeader(ctx *fasthttp.RequestCtx, err error) *string {
	var reason = "unknown"

	switch err.(type) {

	case *openapi3filter.ResponseError:
		responseError, ok := err.(*openapi3filter.ResponseError)

		if ok && responseError.Reason != "" {
			reason = responseError.Reason
		}

		id := fmt.Sprintf("response-%d-%s", ctx.Response.StatusCode(), strings.Split(string(ctx.Response.Header.ContentType()), ";")[0])
		value := fmt.Sprintf("%s:%s:response", id, reason)
		return &value

	case *openapi3filter.RequestError:

		requestError, ok := err.(*openapi3filter.RequestError)
		if !ok {
			return nil
		}

		if requestError.Reason != "" {
			reason = requestError.Reason
		}

		if requestError.Parameter != nil {
			paramName := "request-parameter"

			if requestError.Reason == "" {
				schemaError, ok := requestError.Err.(*openapi3.SchemaError)
				if ok && schemaError.Reason != "" {
					reason = schemaError.Reason
				}
				paramName = requestError.Parameter.Name
			}

			value := fmt.Sprintf("request-parameter:%s:%s", reason, paramName)
			return &value
		}

		if requestError.RequestBody != nil {
			id := fmt.Sprintf("request-body-%s", strings.Split(string(ctx.Request.Header.ContentType()), ";")[0])
			value := fmt.Sprintf("%s:%s:request-body", id, reason)
			return &value
		}
	case *openapi3filter.SecurityRequirementsError:

		secSchemeName := ""
		for _, scheme := range err.(*openapi3filter.SecurityRequirementsError).SecurityRequirements {
			for key := range scheme {
				secSchemeName += key + ","
			}
		}

		secErrors := ""
		for _, secError := range err.(*openapi3filter.SecurityRequirementsError).Errors {
			secErrors += secError.Error() + ","
		}

		value := fmt.Sprintf("security-requirements-%s:%s:%s", strings.TrimSuffix(secSchemeName, ","), strings.TrimSuffix(secErrors, ","), strings.TrimSuffix(secSchemeName, ","))
		return &value
	}

	return nil
}

// Proxy request
func performProxy(ctx *fasthttp.RequestCtx, logger *logrus.Logger, client proxy.HTTPClient) error {
	if err := client.Do(&ctx.Request, &ctx.Response); err != nil {
		logger.WithFields(logrus.Fields{
			"error":      err,
			"request_id": fmt.Sprintf("#%016X", ctx.ID()),
		}).Error("error while proxying request")
		switch err {
		case fasthttp.ErrDialTimeout:
			return web.RespondError(ctx, fasthttp.StatusGatewayTimeout, nil)
		case fasthttp.ErrNoFreeConns:
			return web.RespondError(ctx, fasthttp.StatusServiceUnavailable, nil)
		default:
			return web.RespondError(ctx, fasthttp.StatusBadGateway, nil)
		}
	}

	return nil
}

func (s *openapiWaf) openapiWafHandler(ctx *fasthttp.RequestCtx) error {

	client, err := s.proxyPool.Get()
	if err != nil {
		s.logger.WithFields(logrus.Fields{
			"error":      err,
			"request_id": fmt.Sprintf("#%016X", ctx.ID()),
		}).Error("error while proxying request")
		return web.RespondError(ctx, fasthttp.StatusServiceUnavailable, nil)
	}
	defer s.proxyPool.Put(client)

	// Proxy request if APIFW is disabled
	if s.cfg.RequestValidation == web.ValidationDisable && s.cfg.ResponseValidation == web.ValidationDisable {
		return performProxy(ctx, s.logger, client)
	}

	// If Validation is BLOCK for request and response then respond by CustomBlockStatusCode
	if s.route == nil {
		if s.cfg.RequestValidation == web.ValidationBlock || s.cfg.ResponseValidation == web.ValidationBlock {
			if s.cfg.AddValidationStatusHeader {
				vh := "request: route not found"
				return web.RespondError(ctx, s.cfg.CustomBlockStatusCode, &vh)
			}
			return web.RespondError(ctx, s.cfg.CustomBlockStatusCode, nil)
		}

		// Check shadow api if path or method are not found and validation mode is LOG_ONLY
		if s.cfg.RequestValidation == web.ValidationLog || s.cfg.ResponseValidation == web.ValidationLog {
			// Check Shadow API endpoints
			err := performProxy(ctx, s.logger, client)
			if sErr := s.shadowAPI.Check(ctx); sErr != nil {
				s.logger.WithFields(logrus.Fields{
					"error":      err,
					"request_id": fmt.Sprintf("#%016X", ctx.ID()),
				}).Error("Shadow API check error")
			}
			return err
		}
	}

	var pathParams map[string]string

	if s.pathParamLength > 0 {
		pathParams = make(map[string]string, s.pathParamLength)

		ctx.VisitUserValues(func(key []byte, value interface{}) {
			keyStr := strconv.B2S(key)
			pathParams[keyStr] = value.(string)
		})
	}

	// Convert fasthttp request to net/http request
	req := http.Request{}
	if err := fasthttpadaptor.ConvertRequest(ctx, &req, false); err != nil {
		s.logger.WithFields(logrus.Fields{
			"error":      err,
			"request_id": fmt.Sprintf("#%016X", ctx.ID()),
		}).Error("error while converting http request")
		return web.RespondError(ctx, fasthttp.StatusBadRequest, nil)
	}

	// Validate request
	requestValidationInput := &openapi3filter.RequestValidationInput{
		Request:    &req,
		PathParams: pathParams,
		Route:      s.route,
		Options: &openapi3filter.Options{
			AuthenticationFunc: func(ctx context.Context, input *openapi3filter.AuthenticationInput) error {
				switch input.SecurityScheme.Type {
				case "http":
					switch input.SecurityScheme.Scheme {
					case "basic":
						bHeader := input.RequestValidationInput.Request.Header.Get("Authorization")
						if bHeader == "" || !strings.HasPrefix(strings.ToLower(bHeader), "basic ") {
							return errors.New("missing basic authorization header")
						}
					case "bearer":
						bHeader := input.RequestValidationInput.Request.Header.Get("Authorization")
						if bHeader == "" || !strings.HasPrefix(strings.ToLower(bHeader), "bearer ") {
							return errors.New("missing bearer authorization header")
						}
					}
				case "oauth2", "openIdConnect":
					if s.oauthValidator == nil {
						return errors.New("oauth2 validator not configured")
					}
					if err := s.oauthValidator.Validate(ctx, input.RequestValidationInput.Request.Header.Get("Authorization"), input.Scopes); err != nil {
						return fmt.Errorf("oauth2 error: %s", err)
					}

				case "apiKey":
					switch input.SecurityScheme.In {
					case "header":
						if input.RequestValidationInput.Request.Header.Get(input.SecurityScheme.Name) == "" {
							return fmt.Errorf("missing %s header", input.SecurityScheme.Name)
						}
					case "query":
						if input.RequestValidationInput.Request.URL.Query().Get(input.SecurityScheme.Name) == "" {
							return fmt.Errorf("missing %s query parameter", input.SecurityScheme.Name)
						}
					case "cookie":
						_, err := input.RequestValidationInput.Request.Cookie(input.SecurityScheme.Name)
						if err != nil {
							return fmt.Errorf("missing %s cookie", input.SecurityScheme.Name)
						}
					}
				}
				return nil
			},
		},
	}

	// Get fastjson parser
	jsonParser := s.parserPool.Get()
	defer s.parserPool.Put(jsonParser)

	switch s.cfg.RequestValidation {
	case web.ValidationBlock:
		if err := validator.ValidateRequest(ctx, requestValidationInput, jsonParser); err != nil {
			s.logger.WithFields(logrus.Fields{
				"error":      err,
				"request_id": fmt.Sprintf("#%016X", ctx.ID()),
			}).Error("request validation error")
			if s.cfg.AddValidationStatusHeader {
				if vh := getValidationHeader(ctx, err); vh != nil {
					s.logger.WithFields(logrus.Fields{
						"error":      err,
						"request_id": fmt.Sprintf("#%016X", ctx.ID()),
					}).Errorf("add header %s: %s", web.ValidationStatus, *vh)
					ctx.Request.Header.Add(web.ValidationStatus, *vh)
					return web.RespondError(ctx, s.cfg.CustomBlockStatusCode, vh)
				}
			}
			return web.RespondError(ctx, s.cfg.CustomBlockStatusCode, nil)
		}
	case web.ValidationLog:
		if err := validator.ValidateRequest(ctx, requestValidationInput, jsonParser); err != nil {
			s.logger.WithFields(logrus.Fields{
				"error":      err,
				"request_id": fmt.Sprintf("#%016X", ctx.ID()),
			}).Error("request validation error")
		}
	}

	if err := performProxy(ctx, s.logger, client); err != nil {
		return err
	}

	// Prepare http response headers
	respHeader := http.Header{}
	ctx.Request.Header.VisitAll(func(k, v []byte) {
		sk := string(k)
		sv := string(v)

		respHeader.Set(sk, sv)
	})

	responseValidationInput := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: requestValidationInput,
		Status:                 ctx.Response.StatusCode(),
		Header:                 respHeader,
		Body:                   io.NopCloser(bytes.NewReader(ctx.Response.Body())),
		Options: &openapi3filter.Options{
			ExcludeRequestBody:    false,
			ExcludeResponseBody:   false,
			IncludeResponseStatus: true,
			MultiError:            false,
			AuthenticationFunc:    nil,
		},
	}

	// Validate response
	switch s.cfg.ResponseValidation {
	case web.ValidationBlock:
		if err := validator.ValidateResponse(ctx, responseValidationInput, jsonParser); err != nil {
			s.logger.WithFields(logrus.Fields{
				"error":      err,
				"request_id": fmt.Sprintf("#%016X", ctx.ID()),
			}).Error("response validation error")
			if s.cfg.AddValidationStatusHeader {
				if vh := getValidationHeader(ctx, err); vh != nil {
					s.logger.WithFields(logrus.Fields{
						"error":      err,
						"request_id": fmt.Sprintf("#%016X", ctx.ID()),
					}).Errorf("add header %s: %s", web.ValidationStatus, *vh)
					ctx.Response.Header.Add(web.ValidationStatus, *vh)
					return web.RespondError(ctx, s.cfg.CustomBlockStatusCode, vh)
				}
			}
			return web.RespondError(ctx, s.cfg.CustomBlockStatusCode, nil)
		}
	case web.ValidationLog:
		if err := validator.ValidateResponse(ctx, responseValidationInput, jsonParser); err != nil {
			s.logger.WithFields(logrus.Fields{
				"error":      err,
				"request_id": fmt.Sprintf("#%016X", ctx.ID()),
			}).Error("response validation error")
		}
	}

	return nil
}
