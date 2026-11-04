package xenorchestra

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Client is a minimal client for the XO REST API.
type Client struct {
	// BaseURL is the XO URL, such as https://xo.example.org.
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// NewClient returns a client for the XO at endpoint (such as
// https://xo.example.org), authenticated with an XO token. insecure skips
// the TLS verification (self-signed certificates).
func NewClient(endpoint, token string, insecure bool) *Client {
	httpClient := &http.Client{}
	if insecure {
		httpClient.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	}
	return &Client{BaseURL: strings.TrimSuffix(endpoint, "/"), Token: token, HTTP: httpClient}
}

// do sends a request to /rest/v0<path> and decodes the JSON response into
// out when out is not nil.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	resp, err := c.send(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("xenorchestra: %s %s: decoding response: %w", method, path, err)
	}
	return nil
}

// send returns the response of a successful request; the caller closes it.
func (c *Client) send(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+"/rest/v0"+path, reader)
	if err != nil {
		return nil, err
	}
	req.AddCookie(&http.Cookie{Name: "authenticationToken", Value: c.Token})
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("xenorchestra: %s %s: %w", method, path, err)
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("xenorchestra: %s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(msg)))
	}
	return resp, nil
}

// object is the subset of fields read from XO objects.
type object struct {
	ID        string   `json:"id"`
	UUID      string   `json:"uuid"`
	NameLabel string   `json:"name_label"`
	Pool      string   `json:"$pool"`
	Tags      []string `json:"tags"`
}

// list returns a collection with the fields of object.
func (c *Client) list(ctx context.Context, collection string) ([]object, error) {
	var objs []object
	err := c.do(ctx, http.MethodGet, "/"+collection+"?fields="+url.QueryEscape("id,uuid,name_label,$pool,tags"), nil, &objs)
	return objs, err
}

// action runs POST /<path>/actions/<name> synchronously.
func (c *Client) action(ctx context.Context, path, name string, body, out any) error {
	return c.do(ctx, http.MethodPost, path+"/actions/"+name+"?sync=true", body, out)
}
