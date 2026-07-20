package nlscdownload

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// NewCDPHTTPClient returns an HTTP client whose GET requests are loaded by an
// already-open NLSC browser page through Chrome DevTools Protocol.
func NewCDPHTTPClient(endpoint string, concurrency int, timeout time.Duration) (*http.Client, func(), error) {
	if concurrency < 1 {
		concurrency = 1
	}
	websocketURL, err := findNLSCTarget(endpoint, timeout)
	if err != nil {
		return nil, nil, err
	}
	transport := &cdpTransport{connections: make(chan *cdpConnection, concurrency), timeout: timeout}
	for i := 0; i < concurrency; i++ {
		connection, err := openCDPConnection(websocketURL, timeout)
		if err != nil {
			transport.close()
			return nil, nil, err
		}
		transport.connections <- connection
	}
	return &http.Client{Transport: transport, Timeout: timeout}, transport.close, nil
}

type cdpTarget struct {
	Type                 string `json:"type"`
	URL                  string `json:"url"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

func findNLSCTarget(endpoint string, timeout time.Duration) (string, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid CDP URL %q", endpoint)
	}
	parsed.Path = "/json"
	parsed.RawQuery = ""
	response, err := (&http.Client{Timeout: timeout}).Get(parsed.String())
	if err != nil {
		return "", fmt.Errorf("list CDP targets: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("list CDP targets: HTTP %s", response.Status)
	}
	var targets []cdpTarget
	if err := json.NewDecoder(response.Body).Decode(&targets); err != nil {
		return "", fmt.Errorf("decode CDP targets: %w", err)
	}
	for _, target := range targets {
		if target.Type == "page" && strings.HasPrefix(target.URL, "https://3dmaps.nlsc.gov.tw/") && target.WebSocketDebuggerURL != "" {
			return target.WebSocketDebuggerURL, nil
		}
	}
	return "", errors.New("no open https://3dmaps.nlsc.gov.tw page found in CDP")
}

type cdpConnection struct {
	websocket *websocket.Conn
	frameID   string
	timeout   time.Duration
	nextID    int
}

func openCDPConnection(websocketURL string, timeout time.Duration) (*cdpConnection, error) {
	connection, _, err := websocket.DefaultDialer.Dial(websocketURL, nil)
	if err != nil {
		return nil, fmt.Errorf("connect CDP target: %w", err)
	}
	client := &cdpConnection{websocket: connection, timeout: timeout}
	var frameTree struct {
		FrameTree struct {
			Frame struct {
				ID string `json:"id"`
			} `json:"frame"`
		} `json:"frameTree"`
	}
	if err := client.call("Page.getFrameTree", nil, &frameTree); err != nil {
		connection.Close()
		return nil, err
	}
	if frameTree.FrameTree.Frame.ID == "" {
		connection.Close()
		return nil, errors.New("CDP page has no main frame")
	}
	client.frameID = frameTree.FrameTree.Frame.ID
	return client, nil
}

func (connection *cdpConnection) call(method string, params interface{}, result interface{}) error {
	connection.nextID++
	id := connection.nextID
	deadline := time.Now().Add(connection.timeout)
	_ = connection.websocket.SetWriteDeadline(deadline)
	if err := connection.websocket.WriteJSON(map[string]interface{}{"id": id, "method": method, "params": params}); err != nil {
		return err
	}
	_ = connection.websocket.SetReadDeadline(deadline)
	for {
		var response struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := connection.websocket.ReadJSON(&response); err != nil {
			return err
		}
		if response.ID != id {
			continue
		}
		if response.Error != nil {
			return errors.New(response.Error.Message)
		}
		if result == nil {
			return nil
		}
		return json.Unmarshal(response.Result, result)
	}
}

type cdpTransport struct {
	connections chan *cdpConnection
	timeout     time.Duration
	closeOnce   sync.Once
}

func (transport *cdpTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	var connection *cdpConnection
	select {
	case connection = <-transport.connections:
	case <-request.Context().Done():
		return nil, request.Context().Err()
	}
	defer func() { transport.connections <- connection }()

	var loaded struct {
		Resource struct {
			Success        bool              `json:"success"`
			HTTPStatusCode int               `json:"httpStatusCode"`
			Stream         string            `json:"stream"`
			Headers        map[string]string `json:"headers"`
		} `json:"resource"`
	}
	params := map[string]interface{}{
		"frameId": connection.frameID,
		"url":     request.URL.String(),
		"options": map[string]interface{}{"disableCache": false, "includeCredentials": true},
	}
	if err := connection.call("Network.loadNetworkResource", params, &loaded); err != nil {
		return nil, err
	}
	if !loaded.Resource.Success {
		return nil, errors.New("CDP failed to load network resource")
	}
	body, err := readCDPStream(connection, loaded.Resource.Stream)
	if err != nil {
		return nil, err
	}
	statusCode := loaded.Resource.HTTPStatusCode
	if statusCode == 0 {
		statusCode = http.StatusOK
	}
	headers := make(http.Header)
	for name, value := range loaded.Resource.Headers {
		headers.Set(name, value)
	}
	if statusCode == http.StatusOK && !isGzip(body) {
		body, err = gzipBytes(body)
		if err != nil {
			return nil, err
		}
	}
	headers.Del("Content-Length")
	if statusCode == http.StatusOK {
		headers.Set("Content-Encoding", "gzip")
	}
	return &http.Response{
		Status:        fmt.Sprintf("%d %s", statusCode, http.StatusText(statusCode)),
		StatusCode:    statusCode,
		Header:        headers,
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       request,
	}, nil
}

func readCDPStream(connection *cdpConnection, handle string) ([]byte, error) {
	if handle == "" {
		return nil, nil
	}
	defer connection.call("IO.close", map[string]interface{}{"handle": handle}, nil)
	var body bytes.Buffer
	for {
		var part struct {
			Data          string `json:"data"`
			Base64Encoded bool   `json:"base64Encoded"`
			EOF           bool   `json:"eof"`
		}
		if err := connection.call("IO.read", map[string]interface{}{"handle": handle}, &part); err != nil {
			return nil, err
		}
		if part.Base64Encoded {
			decoded, err := base64.StdEncoding.DecodeString(part.Data)
			if err != nil {
				return nil, err
			}
			body.Write(decoded)
		} else {
			body.WriteString(part.Data)
		}
		if part.EOF {
			return body.Bytes(), nil
		}
	}
}

func isGzip(data []byte) bool {
	return len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b
}

func gzipBytes(data []byte) ([]byte, error) {
	var encoded bytes.Buffer
	writer := gzip.NewWriter(&encoded)
	if _, err := writer.Write(data); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return encoded.Bytes(), nil
}

func (transport *cdpTransport) close() {
	transport.closeOnce.Do(func() {
		for i := 0; i < cap(transport.connections); i++ {
			connection := <-transport.connections
			_ = connection.websocket.Close()
		}
	})
}

var _ http.RoundTripper = (*cdpTransport)(nil)
