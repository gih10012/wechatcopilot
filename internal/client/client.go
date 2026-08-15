package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/gih10012/wechatcopilot/internal/api"
)

type Client struct {
	socket string
	http   *http.Client
}

func New(socket string) *Client {
	dialer := &net.Dialer{Timeout: 2 * time.Second}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socket)
		},
		DisableKeepAlives: false,
	}
	return &Client{socket: socket, http: &http.Client{Transport: transport, Timeout: 5 * time.Minute}}
}

func (c *Client) Get(ctx context.Context, path string, out any) error {
	return c.call(ctx, http.MethodGet, path, nil, out)
}

func (c *Client) Post(ctx context.Context, path string, input, out any) error {
	return c.call(ctx, http.MethodPost, path, input, out)
}

func (c *Client) call(ctx context.Context, method, path string, input, out any) error {
	var body io.Reader
	if input != nil {
		contents, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(contents)
	}
	request, err := http.NewRequestWithContext(ctx, method, "http://unix"+path, body)
	if err != nil {
		return err
	}
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(request)
	if err != nil {
		return api.WrapError(http.StatusServiceUnavailable, api.CodeDaemonUnavailable, "wechatcopilot daemon is unavailable", err)
	}
	defer response.Body.Close()
	var envelope api.Response
	decoder := json.NewDecoder(io.LimitReader(response.Body, 32<<20))
	if err := decoder.Decode(&envelope); err != nil {
		return fmt.Errorf("decode daemon response: %w", err)
	}
	if !envelope.OK {
		if envelope.Error == nil {
			return errors.New("daemon returned an unspecified error")
		}
		return &api.AppError{Status: response.StatusCode, Code: envelope.Error.Code, Message: envelope.Error.Message, Details: envelope.Error.Details}
	}
	if out == nil || envelope.Data == nil {
		return nil
	}
	contents, err := json.Marshal(envelope.Data)
	if err != nil {
		return err
	}
	return json.Unmarshal(contents, out)
}
