/*
 *
 * k6 - a next-generation load testing tool
 * Copyright (C) 2016 Load Impact
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as
 * published by the Free Software Foundation, either version 3 of the
 * License, or (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package http

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/tidwall/gjson"

	"github.com/dop251/goja"
	"github.com/loadimpact/k6/js/common"
	"github.com/loadimpact/k6/js/modules/k6/html"
	"github.com/loadimpact/k6/lib/netext"
)

// ResponseTimings is a struct to put all timings for a given HTTP response/request
type ResponseTimings struct {
	Duration       float64 `json:"duration"`
	Blocked        float64 `json:"blocked"`
	LookingUp      float64 `json:"looking_up"`
	Connecting     float64 `json:"connecting"`
	TLSHandshaking float64 `json:"tls_handshaking"`
	Sending        float64 `json:"sending"`
	Waiting        float64 `json:"waiting"`
	Receiving      float64 `json:"receiving"`
}

// Response is a representation of an HTTP response to be returned to the goja VM
type Response struct {
	ctx context.Context

	RemoteIP       string                   `json:"remote_ip"`
	RemotePort     int                      `json:"remote_port"`
	URL            string                   `json:"url"`
	Status         int                      `json:"status"`
	Proto          string                   `json:"proto"`
	Headers        map[string]string        `json:"headers"`
	Cookies        map[string][]*HTTPCookie `json:"cookies"`
	Body           interface{}              `json:"body"`
	Timings        ResponseTimings          `json:"timings"`
	TLSVersion     string                   `json:"tls_version"`
	TLSCipherSuite string                   `json:"tls_cipher_suite"`
	OCSP           netext.OCSP              `js:"ocsp" json:"ocsp"`
	Error          string                   `json:"error"`
	Request        Request                  `json:"request"`

	cachedJSON    goja.Value
	validatedJSON bool
}

func (res *Response) setTLSInfo(tlsState *tls.ConnectionState) {
	tlsInfo, oscp := netext.ParseTLSConnState(tlsState)
	res.TLSVersion = tlsInfo.Version
	res.TLSCipherSuite = tlsInfo.CipherSuite
	res.OCSP = oscp
}

// JSON parses the body of a response as json and returns it to the goja VM
func (res *Response) JSON(selector ...string) goja.Value {
	hasSelector := len(selector) > 0
	if res.cachedJSON == nil || hasSelector {
		var v interface{}
		var body []byte
		switch b := res.Body.(type) {
		case []byte:
			body = b
		case string:
			body = []byte(b)
		default:
			common.Throw(common.GetRuntime(res.ctx), errors.New("invalid response type"))
		}

		if hasSelector {

			if !res.validatedJSON {
				if !gjson.ValidBytes(body) {
					return goja.Undefined()
				}
				res.validatedJSON = true
			}

			result := gjson.GetBytes(body, selector[0])

			if !result.Exists() {
				return goja.Undefined()
			}
			return common.GetRuntime(res.ctx).ToValue(result.Value())
		}

		if err := json.Unmarshal(body, &v); err != nil {
			common.Throw(common.GetRuntime(res.ctx), err)
		}
		res.validatedJSON = true
		res.cachedJSON = common.GetRuntime(res.ctx).ToValue(v)
	}
	return res.cachedJSON
}

// HTML returns the body as an html.Selection
func (res *Response) HTML(selector ...string) html.Selection {
	var body string
	switch b := res.Body.(type) {
	case []byte:
		body = string(b)
	case string:
		body = b
	default:
		common.Throw(common.GetRuntime(res.ctx), errors.New("invalid response type"))
	}

	sel, err := html.HTML{}.ParseHTML(res.ctx, body)
	if err != nil {
		common.Throw(common.GetRuntime(res.ctx), err)
	}
	sel.URL = res.URL
	if len(selector) > 0 {
		sel = sel.Find(selector[0])
	}
	return sel
}

// SubmitForm parses the body as an html looking for a from and then submitting it
// TODO: document the actual arguments that can be provided
func (res *Response) SubmitForm(args ...goja.Value) (*Response, error) {
	rt := common.GetRuntime(res.ctx)

	formSelector := "form"
	submitSelector := "[type=\"submit\"]"
	var fields map[string]goja.Value
	requestParams := goja.Null()
	if len(args) > 0 {
		params := args[0].ToObject(rt)
		for _, k := range params.Keys() {
			switch k {
			case "formSelector":
				formSelector = params.Get(k).String()
			case "submitSelector":
				submitSelector = params.Get(k).String()
			case "fields":
				if rt.ExportTo(params.Get(k), &fields) != nil {
					fields = nil
				}
			case "params":
				requestParams = params.Get(k)
			}
		}
	}

	form := res.HTML(formSelector)
	if form.Size() == 0 {
		common.Throw(rt, fmt.Errorf("no form found for selector '%s' in response '%s'", formSelector, res.URL))
	}

	methodAttr := form.Attr("method")
	var requestMethod string
	if methodAttr == goja.Undefined() {
		// Use GET by default
		requestMethod = HTTP_METHOD_GET
	} else {
		requestMethod = strings.ToUpper(methodAttr.String())
	}

	responseURL, err := url.Parse(res.URL)
	if err != nil {
		common.Throw(rt, err)
	}

	actionAttr := form.Attr("action")
	var requestURL *url.URL
	if actionAttr == goja.Undefined() {
		// Use the url of the response if no action is set
		requestURL = responseURL
	} else {
		actionURL, err := url.Parse(actionAttr.String())
		if err != nil {
			common.Throw(rt, err)
		}
		requestURL = responseURL.ResolveReference(actionURL)
	}

	// Set the body based on the form values
	values := form.SerializeObject()

	// Set the name + value of the submit button
	submit := form.Find(submitSelector)
	submitName := submit.Attr("name")
	submitValue := submit.Val()
	if submitName != goja.Undefined() && submitValue != goja.Undefined() {
		values[submitName.String()] = submitValue
	}

	// Set the values supplied in the arguments, overriding automatically set values
	for k, v := range fields {
		values[k] = v
	}

	if requestMethod == HTTP_METHOD_GET {
		q := url.Values{}
		for k, v := range values {
			q.Add(k, v.String())
		}
		requestURL.RawQuery = q.Encode()
		return New().Request(res.ctx, requestMethod, rt.ToValue(requestURL.String()), goja.Null(), requestParams)
	}
	return New().Request(res.ctx, requestMethod, rt.ToValue(requestURL.String()), rt.ToValue(values), requestParams)
}

// ClickLink parses the body as an html, looks for a link and than makes a request as if the link was
// clicked
func (res *Response) ClickLink(args ...goja.Value) (*Response, error) {
	rt := common.GetRuntime(res.ctx)

	selector := "a[href]"
	requestParams := goja.Null()
	if len(args) > 0 {
		params := args[0].ToObject(rt)
		for _, k := range params.Keys() {
			switch k {
			case "selector":
				selector = params.Get(k).String()
			case "params":
				requestParams = params.Get(k)
			}
		}
	}

	responseURL, err := url.Parse(res.URL)
	if err != nil {
		common.Throw(rt, err)
	}

	link := res.HTML(selector)
	if link.Size() == 0 {
		common.Throw(rt, fmt.Errorf("no element found for selector '%s' in response '%s'", selector, res.URL))
	}
	hrefAttr := link.Attr("href")
	if hrefAttr == goja.Undefined() {
		common.Throw(rt, fmt.Errorf("no valid href attribute value found on element '%s' in response '%s'", selector, res.URL))
	}
	hrefURL, err := url.Parse(hrefAttr.String())
	if err != nil {
		common.Throw(rt, err)
	}
	requestURL := responseURL.ResolveReference(hrefURL)

	return New().Get(res.ctx, rt.ToValue(requestURL.String()), requestParams)
}
