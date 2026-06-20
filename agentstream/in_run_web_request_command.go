package agentstream

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	pb "github.com/runos-official/nodeagent/l2sec"
	"github.com/runos-official/nodeagent/roslog"
)

// RunWebRequestType is the instruction type that performs an outbound HTTP request.
const RunWebRequestType = "RUN_WEB_REQUEST"

// webRequestTimeout bounds the total time for a RUN_WEB_REQUEST (connect +
// request + response), so a slow or hung endpoint cannot tie up a worker.
const webRequestTimeout = 30 * time.Second

// webRequestMaxBodyBytes caps how much of the response body is read, so a
// hostile or runaway endpoint cannot exhaust memory via an unbounded body.
const webRequestMaxBodyBytes = 32 << 20 // 32 MiB

type runWebRequest struct {
	Url      string `json:"url"`
	Method   string `json:"method"`
	PostData string `json:"postData"`
	// AllowInsecure is retained for wire compatibility but is intentionally
	// ignored: TLS certificate verification is always ON. A caller-controlled
	// knob that disables verification is itself an attack surface (it would let
	// a MITM impersonate any endpoint, including internal/metadata ones), so it
	// is hard-gated off rather than honoured.
	AllowInsecure bool              `json:"allowInsecure"`
	Headers       map[string]string `json:"headers"`
}

type runWebRequestResponse struct {
	ResponseBody       string `json:"responseBody"`
	ResponseStatusCode string `json:"responseStatusCode"`
}

// HandleRunWebRequest performs the HTTP request described by a RUN_WEB_REQUEST
// instruction and returns the response body and status.
func HandleRunWebRequest(instruction *pb.ToNodeAgent) (*pb.FromNodeAgent, error) {
	roslog.I("Executing HandleRunWebRequest")
	jsonData, err := base64.StdEncoding.DecodeString(instruction.JsonB64)
	if err != nil {
		roslog.E("Error decoding JSON payload", err)
		return nil, err
	}

	var request runWebRequest
	if err := json.Unmarshal(jsonData, &request); err != nil {
		roslog.E("Error unmarshalling JSON payload", err)
		return nil, err
	}

	// SSRF guard: require http/https, resolve the host, and reject any request
	// whose resolved IP is loopback, link-local, or the cloud metadata address.
	// We resolve here and pin the dial to the validated IP below, so a hostile
	// DNS record cannot rebind to an internal address between check and connect.
	parsedURL, resolvedIPs, err := validateOutboundURL(request.Url, false)
	if err != nil {
		roslog.E("Rejected RUN_WEB_REQUEST: URL failed SSRF validation", err)
		return nil, err
	}

	// Pick the first validated IP to dial. All resolvedIPs already passed the
	// block check above, so any is safe; pinning to one defeats DNS-rebinding.
	pinnedIP := resolvedIPs[0]

	// Custom dialer that ignores the (re-)resolved host and always connects to
	// the IP we validated, preserving the original port from the URL.
	dialer := &net.Dialer{Timeout: webRequestTimeout}
	dialContext := func(ctx context.Context, network, addr string) (net.Conn, error) {
		_, port, splitErr := net.SplitHostPort(addr)
		if splitErr != nil {
			return nil, fmt.Errorf("invalid dial address %q: %w", addr, splitErr)
		}
		// Re-validate the pinned IP defensively before dialing.
		if isBlockedIP(pinnedIP) {
			return nil, fmt.Errorf("refusing to dial internal/metadata address %s", pinnedIP)
		}
		return dialer.DialContext(ctx, network, net.JoinHostPort(pinnedIP.String(), port))
	}

	// TLS verification is always on. ServerName is left to the default (the URL
	// host) so cert validation still matches the intended hostname even though
	// we dial a pinned IP.
	client := &http.Client{
		Timeout: webRequestTimeout,
		Transport: &http.Transport{
			DialContext: dialContext,
		},
	}

	// Create HTTP request
	var reqBody io.Reader
	if request.PostData != "" {
		reqBody = bytes.NewBufferString(request.PostData)
	}

	method := "GET"
	if request.Method != "" {
		method = request.Method
	}

	req, err := http.NewRequest(method, parsedURL.String(), reqBody)
	if err != nil {
		roslog.E("Error creating request", err)
		return nil, err
	}

	// Add headers
	if request.Headers != nil {
		for key, value := range request.Headers {
			req.Header.Add(key, value)
		}
	}

	// Execute request
	resp, err := client.Do(req)
	if err != nil {
		roslog.E("Error executing request", err)
		return nil, err
	}
	defer resp.Body.Close()

	// Read response body, capped so a hostile/runaway endpoint cannot exhaust memory.
	body, err := io.ReadAll(io.LimitReader(resp.Body, webRequestMaxBodyBytes))
	if err != nil {
		roslog.E("Error reading response body", err)
		return nil, err
	}

	response := runWebRequestResponse{
		ResponseBody:       string(body),
		ResponseStatusCode: strconv.Itoa(resp.StatusCode),
	}

	responseJson, err := json.Marshal(response)
	if err != nil {
		roslog.E("Error marshalling response JSON", err)
		return nil, err
	}
	responseJsonB64 := base64.StdEncoding.EncodeToString(responseJson)

	return &pb.FromNodeAgent{
		JsonB64: responseJsonB64,
		Type:    RunWebRequestType,
	}, nil
}
