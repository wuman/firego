/*
Package firego is a REST client for Firebase (https://firebase.com).
*/
package firego

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	_url "net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/context"
	"golang.org/x/oauth2"
)

// TimeoutDuration is the length of time any request will have to establish
// a connection and receive headers from Firebase before returning
// an ErrTimeout error.
var TimeoutDuration = 30 * time.Second

// ErrValueMissing indicates the firebase node is not found used in MustValue.
var ErrValueMissing = errors.New("firebase node missing")

var defaultRedirectLimit = 30

// ErrTimeout is an error type is that is returned if a request
// exceeds the TimeoutDuration configured.
type ErrTimeout struct {
	error
}

// query parameter constants
const (
	accessTokenParam  = "access_token"
	authParam         = "auth"
	shallowParam      = "shallow"
	formatParam       = "format"
	formatVal         = "export"
	orderByParam      = "orderBy"
	limitToFirstParam = "limitToFirst"
	limitToLastParam  = "limitToLast"
	startAtParam      = "startAt"
	endAtParam        = "endAt"
	equalToParam      = "equalTo"
)

// Firebase represents a location in the cloud.
type Firebase struct {
	url      string
	params   _url.Values
	client   *http.Client
	tokenSrc oauth2.TokenSource

	watchMtx     sync.Mutex
	watching     bool
	stopWatching chan struct{}
}

// New creates a new Firebase reference,
// if client is nil, http.DefaultClient is used.
func New(url string, client *http.Client) *Firebase {

	if client == nil {
		var tr *http.Transport
		tr = &http.Transport{
			DisableKeepAlives: true, // https://code.google.com/p/go/issues/detail?id=3514
			Dial: func(network, address string) (net.Conn, error) {
				start := time.Now()
				c, err := net.DialTimeout(network, address, TimeoutDuration)
				tr.ResponseHeaderTimeout = TimeoutDuration - time.Since(start)
				return c, err
			},
		}

		client = &http.Client{
			Transport:     tr,
			CheckRedirect: redirectPreserveHeaders,
		}
	}

	return &Firebase{
		url:          sanitizeURL(url),
		params:       _url.Values{},
		client:       client,
		stopWatching: make(chan struct{}),
	}
}

// AccessToken sets the TokenSource, which is typically obtained via calling
// google.DefaultTokenSource(ctx, scope...).
func (fb *Firebase) AccessToken(tokenSource oauth2.TokenSource) {
	fb.tokenSrc = tokenSource
}

// Auth sets the custom Firebase token used to authenticate to Firebase.
func (fb *Firebase) Auth(token string) {
	fb.params.Set(authParam, token)
}

// Unauth removes the current token being used to authenticate to Firebase.
func (fb *Firebase) Unauth() {
	fb.params.Del(authParam)
}

// Push creates a reference to an auto-generated child location.
func (fb *Firebase) Push(v interface{}) (*Firebase, error) {
	return fb.PushC(nil, v)
}

// Remove the Firebase reference from the cloud.
func (fb *Firebase) Remove() error {
	return fb.RemoveC(nil)
}

// Set the value of the Firebase reference.
func (fb *Firebase) Set(v interface{}) error {
	return fb.SetC(nil, v)
}

// Update the specific child with the given value.
func (fb *Firebase) Update(v interface{}) error {
	return fb.UpdateC(nil, v)
}

// Value gets the value of the Firebase reference.
func (fb *Firebase) Value(v interface{}) error {
	return fb.ValueC(nil, v)
}

// MustValue gets the value of the Firebase reference, if the node is missing,
// return ErrValueMissing.
func (fb *Firebase) MustValue(v interface{}) error {
	return fb.MustValueC(nil, v)
}

// PushC creates a reference to an auto-generated child location with context.
func (fb *Firebase) PushC(ctx context.Context, v interface{}) (*Firebase, error) {
	bytes, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	bytes, err = fb.doRequest(ctx, "POST", bytes)
	if err != nil {
		return nil, err
	}
	var m map[string]string
	if err := json.Unmarshal(bytes, &m); err != nil {
		return nil, err
	}
	return &Firebase{
		url:    fb.url + "/" + m["name"],
		client: fb.client,
	}, err
}

// RemoveC the Firebase reference from the cloud with context.
func (fb *Firebase) RemoveC(ctx context.Context) error {
	_, err := fb.doRequest(ctx, "DELETE", nil)
	if err != nil {
		return err
	}
	return nil
}

// SetC the value of the Firebase reference with context.
func (fb *Firebase) SetC(ctx context.Context, v interface{}) error {
	bytes, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = fb.doRequest(ctx, "PUT", bytes)
	return err
}

// UpdateC the specific child with the given value with context.
func (fb *Firebase) UpdateC(ctx context.Context, v interface{}) error {
	bytes, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = fb.doRequest(ctx, "PATCH", bytes)
	return err
}

// ValueC gets the value of the Firebase reference with context.
func (fb *Firebase) ValueC(ctx context.Context, v interface{}) error {
	bytes, err := fb.doRequest(ctx, "GET", nil)
	if err != nil {
		return err
	}
	return json.Unmarshal(bytes, v)
}

// MustValueC gets the value of the Firebase reference with context, and return
// an ErrValueMissing if the node is not found.
func (fb *Firebase) MustValueC(ctx context.Context, v interface{}) error {
	bytes, err := fb.doRequest(ctx, "GET", nil)
	if err != nil {
		return err
	} else if len(bytes) == 0 || string(bytes) == "null" {
		return ErrValueMissing
	}
	return json.Unmarshal(bytes, v)
}

// String returns the string representation of the
// Firebase reference.
func (fb *Firebase) String() string {
	path := fb.url + "/.json"

	paramsCopy, _ := _url.ParseQuery(fb.params.Encode())
	if fb.tokenSrc != nil {
		if token, err := fb.tokenSrc.Token(); err == nil && token != nil {
			paramsCopy.Add(accessTokenParam, token.AccessToken)
		}
	}

	if len(paramsCopy) > 0 {
		path += "?" + paramsCopy.Encode()
	}
	return path
}

// Child creates a new Firebase reference for the requested
// child with the same configuration as the parent.
func (fb *Firebase) Child(child string) *Firebase {
	c := fb.copy()
	c.url = c.url + "/" + child
	return c
}

func (fb *Firebase) copy() *Firebase {
	c := &Firebase{
		url:          fb.url,
		params:       _url.Values{},
		client:       fb.client,
		stopWatching: make(chan struct{}),
		tokenSrc:     fb.tokenSrc,
	}

	// making sure to manually copy the map items into a new
	// map to avoid modifying the map reference.
	for k, v := range fb.params {
		c.params[k] = v
	}
	return c
}

func sanitizeURL(url string) string {
	if !strings.HasPrefix(url, "https://") && !strings.HasPrefix(url, "http://") {
		url = "https://" + url
	}

	if strings.HasSuffix(url, "/") {
		url = url[:len(url)-1]
	}

	return url
}

// Preserve headers on redirect.
//
// Reference https://github.com/golang/go/issues/4800
func redirectPreserveHeaders(req *http.Request, via []*http.Request) error {
	if len(via) == 0 {
		// No redirects
		return nil
	}

	if len(via) > defaultRedirectLimit {
		return fmt.Errorf("%d consecutive requests(redirects)", len(via))
	}

	// mutate the subsequent redirect requests with the first Header
	for key, val := range via[0].Header {
		req.Header[key] = val
	}
	return nil
}

func (fb *Firebase) doRequest(ctx context.Context, method string, body []byte) ([]byte, error) {
	req, err := http.NewRequest(method, fb.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if ctx != nil {
		req = req.WithContext(ctx)
	}

	resp, err := fb.client.Do(req)
	switch err := err.(type) {
	default:
		return nil, err
	case nil:
		// carry on

	case *_url.Error:
		// `http.Client.Do` will return a `url.Error` that wraps a `net.Error`
		// when exceeding it's `Transport`'s `ResponseHeadersTimeout`
		e1, ok := err.Err.(net.Error)
		if ok && e1.Timeout() {
			return nil, ErrTimeout{err}
		}

		return nil, err

	case net.Error:
		// `http.Client.Do` will return a `net.Error` directly when Dial times
		// out, or when the Client's RoundTripper otherwise returns an err
		if err.Timeout() {
			return nil, ErrTimeout{err}
		}

		return nil, err
	}

	defer resp.Body.Close()
	respBody, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/200 != 1 {
		return nil, errors.New(string(respBody))
	}
	return respBody, nil
}
