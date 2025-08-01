// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package dialer

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"

	"golang.org/x/net/http/httpproxy"
	"google.golang.org/grpc"
)

/*
 * Copyright 2023 gRPC authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

const grpcUA = "grpc-go/" + grpc.Version

// DynamicProxyDialer is a fork of grpc standard dialer which supports dynamic resolving of proxy settings
// on each request (vs. caching it once per process).
//
// DynamicProxyDialer assumes that the address is using 'tcp' network.
func DynamicProxyDialer(ctx context.Context, addr string) (net.Conn, error) {
	newAddr := addr

	proxyURL, err := mapAddress(addr)
	if err != nil {
		return nil, err
	}

	if proxyURL != nil {
		newAddr = proxyURL.Host
	}

	conn, err := NetDialerWithTCPKeepalive().DialContext(ctx, "tcp", newAddr)
	if err != nil {
		return nil, err
	}

	if proxyURL == nil {
		// proxy is disabled if proxyURL is nil.
		return conn, err
	}

	return doHTTPConnectHandshake(ctx, conn, addr, proxyURL, grpcUA)
}

const proxyAuthHeaderKey = "Proxy-Authorization"

func mapAddress(address string) (*url.URL, error) {
	req := &http.Request{
		URL: &url.URL{
			Scheme: "https",
			Host:   address,
		},
	}

	return httpproxy.FromEnvironment().ProxyFunc()(req.URL)
}

// To read a response from a net.Conn, http.ReadResponse() takes a bufio.Reader.
// It's possible that this reader reads more than what's need for the response and stores
// those bytes in the buffer.
// bufConn wraps the original net.Conn and the bufio.Reader to make sure we don't lose the
// bytes in the buffer.
type bufConn struct {
	net.Conn

	r io.Reader
}

func (c *bufConn) Read(b []byte) (int, error) {
	return c.r.Read(b)
}

func basicAuth(username, password string) string {
	auth := username + ":" + password

	return base64.StdEncoding.EncodeToString([]byte(auth))
}

func doHTTPConnectHandshake(ctx context.Context, conn net.Conn, backendAddr string, proxyURL *url.URL, grpcUA string) (_ net.Conn, err error) {
	defer func() {
		if err != nil {
			conn.Close() //nolint:errcheck
		}
	}()

	req := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Host: backendAddr},
		Header: map[string][]string{"User-Agent": {grpcUA}},
	}

	if t := proxyURL.User; t != nil {
		u := t.Username()
		p, _ := t.Password()
		req.Header.Add(proxyAuthHeaderKey, "Basic "+basicAuth(u, p))
	}

	if err := sendHTTPRequest(ctx, req, conn); err != nil {
		return nil, fmt.Errorf("failed to write the HTTP request: %v", err)
	}

	r := bufio.NewReader(conn)

	resp, err := http.ReadResponse(r, req)
	if err != nil {
		return nil, fmt.Errorf("reading server HTTP response: %v", err)
	}

	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		dump, err := httputil.DumpResponse(resp, true)
		if err != nil {
			return nil, fmt.Errorf("failed to do connect handshake, status code: %s", resp.Status)
		}

		return nil, fmt.Errorf("failed to do connect handshake, response: %q", dump)
	}

	// The buffer could contain extra bytes from the target server, so we can't
	// discard it. However, in many cases where the server waits for the client
	// to send the first message (e.g. when TLS is being used), the buffer will
	// be empty, so we can avoid the overhead of reading through this buffer.
	if r.Buffered() != 0 {
		return &bufConn{Conn: conn, r: r}, nil
	}

	return conn, nil
}

func sendHTTPRequest(ctx context.Context, req *http.Request, conn net.Conn) error {
	req = req.WithContext(ctx)
	if err := req.Write(conn); err != nil {
		return fmt.Errorf("failed to write the HTTP request: %v", err)
	}

	return nil
}

// NetDialerWithTCPKeepalive returns a net.Dialer that enables TCP keepalives on
// the underlying connection with OS default values for keepalive parameters.
func NetDialerWithTCPKeepalive() *net.Dialer {
	return &net.Dialer{
		KeepAliveConfig: net.KeepAliveConfig{
			Enable:   true,
			Idle:     -1,
			Count:    -1,
			Interval: -1,
		},
	}
}
