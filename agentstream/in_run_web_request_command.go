package agentstream

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"io"
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
	Url           string            `json:"url"`
	Method        string            `json:"method"`
	PostData      string            `json:"postData"`
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

	// Create custom HTTP client with optional insecure SSL and a total
	// request timeout so a slow/hung endpoint cannot tie up a worker.
	client := &http.Client{
		Timeout: webRequestTimeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: request.AllowInsecure,
			},
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

	req, err := http.NewRequest(method, request.Url, reqBody)
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
