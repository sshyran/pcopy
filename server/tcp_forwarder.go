package server

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"heckel.io/pcopy/util"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"
)

const (
	defaultReadTimeout = 3 * time.Second
	bufferSizeBytes    = 16 * 1024
)

// tcpForwarder is a server that listens on a raw TCP socket and forwards incoming connections to an upstream
// HTTP handler function as a PUT request. That makes it possible to do "cat ... | nc nopaste.net 9999".
type tcpForwarder struct {
	Addr            string
	UpstreamAddr    string
	UpstreamHandler http.HandlerFunc
	ReadTimeout     time.Duration
	cancel          context.CancelFunc
}

func newTCPForwarder(addr string, upstreamAddr string, upstreamHandler http.HandlerFunc) *tcpForwarder {
	return &tcpForwarder{
		Addr:            addr,
		UpstreamAddr:    upstreamAddr,
		UpstreamHandler: upstreamHandler,
		ReadTimeout:     defaultReadTimeout,
	}
}

func (s *tcpForwarder) listenAndServe() error {
	listener, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return err
	}
	defer listener.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				log.Printf("error accepting connection on %s: %s", s.Addr, err.Error())
				continue
			}
			go func(conn net.Conn) {
				defer conn.Close()
				if err := s.handleConn(conn); err != nil {
					io.WriteString(conn, fmt.Sprintf("%s\n", err.Error())) // might fail
					log.Printf("%s - tcp forward error: %s", conn.RemoteAddr().String(), err.Error())
				}
			}(conn)
		}
	}()

	s.cancel = cancel
	<-ctx.Done()
	return nil
}

func (s *tcpForwarder) shutdown() {
	s.cancel()
}

// handleConn reads from the TCP socket and forwards it to the HTTP handler. This method does NOT close the underlying
// connection. This is done in the listenAndServe to ensure that error messages can be sent to the client.
func (s *tcpForwarder) handleConn(conn net.Conn) error {
	// Peak connection to detect "pcopy:..." prefix and extract path
	connReadCloser := &connTimeoutReadCloser{conn: conn, timeout: s.ReadTimeout}
	peaked, err := util.Peak(connReadCloser, bufferSizeBytes)
	if err != nil {
		return fmt.Errorf("cannot peak: %w", err)
	} else if strings.TrimSpace(string(peaked.PeakedBytes)) == "help" {
		return s.handleHelp(conn)
	}
	path, offset := extractPath(peaked.PeakedBytes)

	// Prepare upstream HTTP request
	rawURL := fmt.Sprintf("%s/%s", s.UpstreamAddr, path)
	requestBodyReader, requestBodyWriter := io.Pipe()
	request, err := http.NewRequest(http.MethodPut, rawURL, requestBodyReader)
	if err != nil {
		return fmt.Errorf("cannot create forwarding request: %w", err)
	}
	request.RequestURI = fmt.Sprintf("/%s", path)
	request.RemoteAddr = conn.RemoteAddr().String()
	request.Header.Set(HeaderNoRedirect, "1")

	// Read downstream connection and copy to HTTP request body, including peaked bytes
	errChan := make(chan error)
	go func() {
		requestBody := io.MultiReader(bytes.NewReader(peaked.PeakedBytes[offset:]), connReadCloser)
		_, err := io.Copy(requestBodyWriter, requestBody)
		if err != nil {
			errChan <- err
		} else {
			errChan <- requestBodyWriter.Close() // closing the upstream request will finish ServeHTTP()
		}
	}()

	// Record upstream response and forward downstream
	rr := httptest.NewRecorder()
	s.UpstreamHandler.ServeHTTP(rr, request)
	defer func() {
		requestBodyReader.Close()
		requestBodyWriter.Close()
	}()
	if rr.Code != http.StatusCreated && rr.Code != http.StatusPartialContent {
		return errors.New(rr.Result().Status)
	}
	if err := <-errChan; err != nil {
		return err
	}
	if _, err := conn.Write(rr.Body.Bytes()); err != nil {
		return err
	}
	return nil
}

func (s *tcpForwarder) handleHelp(conn net.Conn) error {
	rawURL := fmt.Sprintf("%s/nc", s.UpstreamAddr)
	request, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Errorf("cannot create forwarding request: %w", err)
	}
	request.RequestURI = "/nc"
	request.RemoteAddr = conn.RemoteAddr().String()
	request.Header.Set(HeaderNoRedirect, "1")
	rr := httptest.NewRecorder()
	s.UpstreamHandler.ServeHTTP(rr, request)
	if rr.Code != http.StatusOK {
		return errors.New(rr.Result().Status)
	}
	if _, err := conn.Write(rr.Body.Bytes()); err != nil {
		return err
	}
	return nil
}

func extractPath(peaked []byte) (string, int) {
	reader := bufio.NewReader(bytes.NewReader(peaked))
	s, err := reader.ReadString('\n')
	if err != nil || !strings.HasPrefix(s, "pcopy:") {
		return "", 0
	}
	return strings.TrimSuffix(strings.TrimPrefix(s, "pcopy:"), "\n"), len(s)
}

type connTimeoutReadCloser struct {
	conn    net.Conn
	timeout time.Duration
	lastErr error
}

func (c *connTimeoutReadCloser) Read(p []byte) (n int, err error) {
	if c.lastErr == io.EOF {
		return 0, io.EOF // No need to wait if we're at the end
	}
	if err := c.conn.SetReadDeadline(time.Now().Add(c.timeout)); err != nil {
		return 0, fmt.Errorf("cannot set read deadline: %w", err)
	}
	read, err := c.conn.Read(p)
	if err != nil && strings.Contains(err.Error(), "i/o timeout") { // poll.DeadlineExceededError is not accessible
		err = io.EOF
	}
	c.lastErr = err
	return read, err
}

func (c *connTimeoutReadCloser) Close() error {
	return c.conn.Close()
}