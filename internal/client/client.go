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

// A surface response can contain a base64-encoded screenshot whose decoded
// size is capped at 32 MiB by the runtime. 48 MiB leaves room for base64 and
// the bounded surface metadata while still placing a hard limit on the local
// daemon protocol.
const maxDaemonResponseBytes int64 = 48 << 20

type responseEnvelope struct {
	SchemaVersion string          `json:"schema_version"`
	OK            bool            `json:"ok"`
	Data          json.RawMessage `json:"data,omitempty"`
	Error         *api.Error      `json:"error,omitempty"`
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
	envelope, err := decodeDaemonResponse(response.Body, maxDaemonResponseBytes)
	if err != nil {
		return err
	}
	if !envelope.OK {
		if envelope.Error == nil {
			return errors.New("daemon returned an unspecified error")
		}
		return &api.AppError{Status: response.StatusCode, Code: envelope.Error.Code, Message: envelope.Error.Message, Details: envelope.Error.Details}
	}
	if out == nil || len(envelope.Data) == 0 || bytes.Equal(envelope.Data, []byte("null")) {
		return nil
	}
	return json.Unmarshal(envelope.Data, out)
}

func decodeDaemonResponse(body io.Reader, maximum int64) (responseEnvelope, error) {
	if maximum <= 0 {
		return responseEnvelope{}, errors.New("decode daemon response: invalid response limit")
	}
	limited := &io.LimitedReader{R: body, N: maximum + 1}
	decoder := json.NewDecoder(limited)
	var envelope responseEnvelope
	decodeErr := decoder.Decode(&envelope)
	var trailing json.RawMessage
	trailingErr := decoder.Decode(&trailing)
	if limited.N == 0 {
		return responseEnvelope{}, fmt.Errorf("decode daemon response: response exceeds %d bytes", maximum)
	}
	if decodeErr != nil {
		return responseEnvelope{}, fmt.Errorf("decode daemon response: %w", decodeErr)
	}
	if !errors.Is(trailingErr, io.EOF) {
		if trailingErr == nil {
			trailingErr = errors.New("multiple JSON values")
		}
		return responseEnvelope{}, fmt.Errorf("decode daemon response: trailing data: %w", trailingErr)
	}
	return envelope, nil
}
