/*
Copyright (c) 2014 VMware, Inc. All Rights Reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package soap

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/vmware/govmomi/vim25/progress"
	"github.com/vmware/govmomi/vim25/types"
	"github.com/vmware/govmomi/vim25/xml"
)

type HasFault interface {
	Fault() *Fault
}

type RoundTripper interface {
	RoundTrip(req, res HasFault) error
}

type Client struct {
	http.Client

	u        url.URL
	insecure bool
	d        *debugContainer
}

func NewClient(u url.URL, insecure bool) *Client {
	c := Client{
		u:        u,
		insecure: insecure,
		d:        newDebug(),
	}

	if c.u.Scheme == "https" {
		c.Client.Transport = &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: c.insecure,
			},
		}
	}

	c.Jar, _ = cookiejar.New(nil)
	c.u.User = nil

	return &c
}

func (c *Client) URL() url.URL {
	return c.u
}

type marshaledClient struct {
	Cookies  []*http.Cookie
	URL      *url.URL
	Insecure bool
}

func (c *Client) MarshalJSON() ([]byte, error) {
	m := marshaledClient{
		Cookies:  c.Jar.Cookies(&c.u),
		URL:      &c.u,
		Insecure: c.insecure,
	}

	return json.Marshal(m)
}

func (c *Client) UnmarshalJSON(b []byte) error {
	var m marshaledClient

	err := json.Unmarshal(b, &m)
	if err != nil {
		return err
	}

	*c = *NewClient(*m.URL, m.Insecure)
	c.Jar.SetCookies(m.URL, m.Cookies)

	return nil
}

func (c *Client) RoundTrip(reqBody, resBody HasFault) error {
	var httpreq *http.Request
	var httpres *http.Response
	var err error

	reqEnv := Envelope{Body: reqBody}
	resEnv := Envelope{Body: resBody}

	// Create debugging context for this round trip
	d := c.d.newRoundTrip()
	if d.enabled() {
		defer d.done()
	}

	b, err := xml.Marshal(reqEnv)
	if err != nil {
		panic(err)
	}

	rawReqBody := io.MultiReader(strings.NewReader(xml.Header), bytes.NewReader(b))
	httpreq, err = http.NewRequest("POST", c.u.String(), rawReqBody)
	if err != nil {
		panic(err)
	}

	httpreq.Header.Set(`Content-Type`, `text/xml; charset="utf-8"`)
	httpreq.Header.Set(`SOAPAction`, `urn:vim25/5.5`)

	if d.enabled() {
		d.debugRequest(httpreq)
	}

	tstart := time.Now()
	httpres, err = c.Client.Do(httpreq)
	tstop := time.Now()

	if d.enabled() {
		d.logf("done in %6dms", tstop.Sub(tstart)/time.Millisecond)
	}

	if err != nil {
		return err
	}

	if d.enabled() {
		d.debugResponse(httpres)
	}

	// Close response regardless of what happens next
	defer httpres.Body.Close()

	dec := xml.NewDecoder(httpres.Body)
	dec.TypeFunc = types.TypeFunc()
	err = dec.Decode(&resEnv)
	if err != nil {
		return err
	}

	if f := resBody.Fault(); f != nil {
		return WrapSoapFault(f)
	}

	return err
}

// ParseURL wraps url.Parse to rewrite the URL.Host field
// In the case of VM guest uploads or NFC lease URLs, a Host
// field with a value of "*" is rewritten to the Client's URL.Host.
func (c *Client) ParseURL(urlStr string) (*url.URL, error) {
	u, err := url.Parse(urlStr)
	if err != nil {
		return nil, err
	}

	host := strings.Split(u.Host, ":")
	if host[0] == "*" {
		// Also use Client's port, to support port forwarding
		u.Host = c.URL().Host
	}

	return u, nil
}

type Upload struct {
	Type          string
	Method        string
	ContentLength int64
	Headers       map[string]string
	Progress      progress.Sinker
}

var DefaultUpload = Upload{
	Type:   "application/octet-stream",
	Method: "PUT",
}

// Upload PUTs the local file to the given URL
func (c *Client) Upload(f io.Reader, u *url.URL, param *Upload) error {
	var err error

	if param.Progress != nil {
		pr := progress.NewReader(param.Progress, f, param.ContentLength)
		f = pr

		// Mark progress reader as done when returning from this function.
		defer func() {
			pr.Done(err)
		}()
	}

	req, err := http.NewRequest(param.Method, u.String(), f)
	if err != nil {
		return err
	}

	req.ContentLength = param.ContentLength
	req.Header.Set("Content-Type", param.Type)

	for k, v := range param.Headers {
		req.Header.Add(k, v)
	}

	res, err := c.Client.Do(req)
	if err != nil {
		return err
	}

	switch res.StatusCode {
	case http.StatusOK:
	case http.StatusCreated:
	default:
		err = errors.New(res.Status)
	}

	return err
}

// UploadFile PUTs the local file to the given URL
func (c *Client) UploadFile(file string, u *url.URL, param *Upload) error {
	if param == nil {
		p := DefaultUpload // Copy since we set ContentLength
		param = &p
	}

	s, err := os.Stat(file)
	if err != nil {
		return err
	}

	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()

	param.ContentLength = s.Size()

	return c.Upload(f, u, param)
}

type Download struct {
	Method   string
	Progress progress.Sinker
}

var DefaultDownload = Download{
	Method: "GET",
}

// DownloadFile GETs the given URL to a local file
func (c *Client) DownloadFile(file string, u *url.URL, param *Download) error {
	var err error

	if param == nil {
		param = &DefaultDownload
	}

	fh, err := os.Create(file)
	if err != nil {
		return err
	}
	defer fh.Close()

	req, err := http.NewRequest(param.Method, u.String(), nil)
	if err != nil {
		return err
	}

	res, err := c.Client.Do(req)
	if err != nil {
		return err
	}

	defer res.Body.Close()

	switch res.StatusCode {
	case http.StatusOK:
	default:
		err = errors.New(res.Status)
	}

	if err != nil {
		return err
	}

	var r io.Reader = res.Body
	if param.Progress != nil {
		pr := progress.NewReader(param.Progress, res.Body, res.ContentLength)
		r = pr

		// Mark progress reader as done when returning from this function.
		defer func() {
			pr.Done(err)
		}()
	}

	_, err = io.Copy(fh, r)
	if err != nil {
		return err
	}

	// Assign error before returning so that it gets picked up by the deferred
	// function marking the progress reader as done.
	err = fh.Close()
	if err != nil {
		return err
	}

	return nil
}
