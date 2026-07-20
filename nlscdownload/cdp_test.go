package nlscdownload

import (
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestCDPHTTPClientLoadsNetworkResourceAsGzip(t *testing.T) {
	const payload = "decoded NODE body"
	var loadedURL string
	server := httptest.NewServer(nil)
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/json":
			_ = json.NewEncoder(w).Encode([]map[string]string{{
				"type": "page", "url": "https://3dmaps.nlsc.gov.tw/FrontMap/",
				"webSocketDebuggerUrl": "ws" + strings.TrimPrefix(server.URL, "http") + "/devtools/page/test",
			}})
		case "/devtools/page/test":
			connection, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
			if err != nil {
				t.Errorf("upgrade websocket: %v", err)
				return
			}
			defer connection.Close()
			for {
				var request struct {
					ID     int             `json:"id"`
					Method string          `json:"method"`
					Params json.RawMessage `json:"params"`
				}
				if err := connection.ReadJSON(&request); err != nil {
					return
				}
				result := interface{}(map[string]interface{}{})
				switch request.Method {
				case "Page.getFrameTree":
					result = map[string]interface{}{"frameTree": map[string]interface{}{"frame": map[string]interface{}{"id": "frame-1"}}}
				case "Network.loadNetworkResource":
					var params struct {
						URL string `json:"url"`
					}
					_ = json.Unmarshal(request.Params, &params)
					loadedURL = params.URL
					result = map[string]interface{}{"resource": map[string]interface{}{"success": true, "httpStatusCode": 200, "stream": "stream-1"}}
				case "IO.read":
					result = map[string]interface{}{"data": base64.StdEncoding.EncodeToString([]byte(payload)), "base64Encoded": true, "eof": true}
				}
				if err := connection.WriteJSON(map[string]interface{}{"id": request.ID, "result": result}); err != nil {
					return
				}
			}
		default:
			http.NotFound(w, r)
		}
	})
	defer server.Close()

	client, closeClient, err := NewCDPHTTPClient(server.URL, 1, time.Second)
	if err != nil {
		t.Fatalf("NewCDPHTTPClient() error = %v", err)
	}
	defer closeClient()
	requestURL := "https://mapserver02.nlsc.gov.tw/oview?name=113_B&level=5&Row=3&Col=2"
	response, err := client.Get(requestURL)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	defer response.Body.Close()
	reader, err := gzip.NewReader(response.Body)
	if err != nil {
		t.Fatalf("gzip.NewReader() error = %v", err)
	}
	decoded, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read gzip response: %v", err)
	}
	if string(decoded) != payload || loadedURL != requestURL {
		t.Fatalf("decoded/URL = %q/%q, want %q/%q", decoded, loadedURL, payload, requestURL)
	}
}
