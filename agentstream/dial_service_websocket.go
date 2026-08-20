package agentstream

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/runos-official/nodeagent/roslog"
)

// The far end for STREAM_TARGET_SERVICE_WEBSOCKET: a plain websocket to a Service inside this
// cluster (goal 31). The web terminal, runostty, is the consumer this is written for, and it is
// the simpler of the two far ends, which is why it is built first.
//
// EVERYTHING HERE WAS READ FROM THE runostty SOURCE, not assumed from ttyd or any other terminal
// server, and two details would have been wrong by assumption:
//
//  1. THE CLIENT MUST OFFER TWO SUBPROTOCOLS, not one. `runos.psk.<PSK>` carries the secret and
//     the server deliberately never selects it, because a selected subprotocol is echoed in a
//     response header and that is the one place the secret must not appear. It selects
//     `runos.tty.v1` instead, and it MUST select something or a browser aborts the connection.
//     A client offering only the psk protocol gets no selection back.
//  2. A MESSAGE OVER 1 MiB IS DROPPED, silently, with only a server-side log line. So input is
//     chunked here rather than handed over whole; see Write.
//
// The secret rides a header and never a query string, which is the far end's own rule: a token in
// a URL is written into the ingress access log, the browser's history and any Referer a page
// leaks. This dialer passes whatever headers conductor supplies and invents none.

const (
	// How long to wait for the upgrade. A Service inside the cluster answers in milliseconds; this
	// is a bound on a hung far end, not a budget anyone should be using.
	wsHandshakeTimeout = 10 * time.Second

	// The far end refuses anything larger, so a paste bigger than this is split rather than lost.
	// One byte under, because the check there is `>` on the whole frame.
	wsMaxFrameBytes = 1024*1024 - 1
)

// serviceWebSocket is one open websocket to a Service, as the session sees it.
type serviceWebSocket struct {
	conn *websocket.Conn
	// Gorilla permits ONE concurrent writer and one concurrent reader. Write, Resize and Close can
	// all reach this from different goroutines, so every writer takes this.
	writeMu sync.Mutex
	closeMu sync.Mutex
	closed  bool
}

// serviceWebSocketTarget turns an OPEN_STREAM into the address, subprotocols and headers to dial.
//
// SPLIT OUT SO IT CAN BE TESTED FOR WHAT IT IS: the in-cluster name is the whole point of this
// goal (nothing about this path leaves the cluster) and it is also the part with no observable
// behaviour to assert from the outside. Keeping it pure means the name can be checked directly,
// and the protocol below can be checked against a real server, without a test-only hook in the
// dial path.
func serviceWebSocketTarget(open StreamOpen) (url.URL, []string, http.Header, error) {
	if open.Target != "" && open.Target != "STREAM_TARGET_SERVICE_WEBSOCKET" && open.Target != "SERVICE_WEBSOCKET" {
		return url.URL{}, nil, nil, fmt.Errorf("this node agent can only open a service websocket, and this session asked for %s", open.Target)
	}
	if open.Name == "" || open.Namespace == "" {
		return url.URL{}, nil, nil, fmt.Errorf("a service websocket needs both a namespace and a service name")
	}
	if open.Port == 0 {
		return url.URL{}, nil, nil, fmt.Errorf("a service websocket needs a port")
	}

	path := open.Path
	if path == "" {
		path = "/"
	} else if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	// The in-cluster name. Dialled from the node, so it resolves through the cluster's own DNS,
	// and nothing about this path leaves the cluster: that is the whole point of the goal.
	target := url.URL{
		Scheme: "ws",
		Host:   fmt.Sprintf("%s.%s.svc.cluster.local:%d", open.Name, open.Namespace, open.Port),
		Path:   path,
	}

	header := http.Header{}
	var subprotocols []string
	for k, v := range open.Headers {
		// Gorilla rejects Sec-WebSocket-Protocol in the header map and takes it as a field instead.
		if strings.EqualFold(k, "Sec-WebSocket-Protocol") {
			for _, p := range strings.Split(v, ",") {
				if p = strings.TrimSpace(p); p != "" {
					subprotocols = append(subprotocols, p)
				}
			}
			continue
		}
		header.Set(k, v)
	}
	return target, subprotocols, header, nil
}

// DialServiceWebSocket opens a websocket to a Service in this cluster and streams it to a session.
//
// Installed as the agent's Dialer at startup. It handles the SERVICE_WEBSOCKET target and refuses
// the others by name, so a VM console asked for on a build that cannot serve one is told exactly
// that rather than opening and staying silent.
func DialServiceWebSocket(sessionID string, open StreamOpen, onData func([]byte), onClose func(string, bool)) (FarEnd, error) {
	target, subprotocols, header, err := serviceWebSocketTarget(open)
	if err != nil {
		return nil, err
	}
	far, err := dialWebSocketAt(target.String(), subprotocols, header, onData, onClose)
	if err != nil {
		return nil, err
	}
	roslog.I("Opened a service websocket", "session", sessionID, "target", target.Host, "path", target.Path)
	return far, nil
}

// dialWebSocketAt is the transport half: everything from the upgrade onwards, with no opinion
// about how the address was arrived at.
func dialWebSocketAt(rawURL string, subprotocols []string, header http.Header, onData func([]byte), onClose func(string, bool)) (*serviceWebSocket, error) {
	dialer := &websocket.Dialer{
		HandshakeTimeout: wsHandshakeTimeout,
		Subprotocols:     subprotocols,
	}

	conn, resp, err := dialer.Dial(rawURL, header)
	if err != nil {
		// The status is the whole diagnosis for the commonest failure: 401 means the secret was
		// wrong or absent, and without it the operator sees only "bad handshake".
		if resp != nil {
			return nil, fmt.Errorf("the far end answered %s to the websocket upgrade: %w", resp.Status, err)
		}
		return nil, fmt.Errorf("could not reach the far end: %w", err)
	}

	far := &serviceWebSocket{conn: conn}

	// The reader is the ONLY place output crosses into the session. It calls onData, which sends
	// on the bulk path SYNCHRONOUSLY, so a far end producing faster than the node's stream drains
	// makes this loop wait. That IS the backpressure: this goroutine stops reading, the websocket's
	// own receive window fills, and the PTY at the far end blocks, which is exactly what a slow
	// terminal should do. Nothing is buffered without bound and nothing is dropped.
	go func() {
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				reason, isError := describeReadEnd(err)
				far.markClosed()
				onClose(reason, isError)
				return
			}
			if len(data) > 0 {
				onData(data)
			}
		}
	}()

	return far, nil
}

// describeReadEnd turns the read error into something an operator can act on, and says whether it
// was a failure or an ordinary hang-up. A shell exiting is not an error.
func describeReadEnd(err error) (string, bool) {
	if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
		return "the session ended", false
	}
	if websocket.IsUnexpectedCloseError(err) {
		return fmt.Sprintf("the far end closed the connection unexpectedly: %v", err), true
	}
	return fmt.Sprintf("the connection to the far end failed: %v", err), true
}

// Write sends what the operator typed.
//
// CHUNKED AT THE FAR END'S LIMIT. runostty drops a message over 1 MiB with nothing but a
// server-side log line, so handing it a large paste whole would lose it silently, and silent loss
// on a byte stream is the one failure this design refuses everywhere else. Splitting is safe
// because the far end writes whatever arrives straight into the PTY.
func (s *serviceWebSocket) Write(p []byte) (int, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	written := 0
	for written < len(p) {
		end := written + wsMaxFrameBytes
		if end > len(p) {
			end = len(p)
		}
		// A TEXT frame, because the far end reads the message as a string and writes that to the
		// PTY. Input is what somebody typed or pasted.
		if err := s.conn.WriteMessage(websocket.TextMessage, p[written:end]); err != nil {
			return written, err
		}
		written = end
	}
	return written, nil
}

// Resize tells the far end the terminal's new size.
//
// A JSON object carrying cols and rows, which is how runostty distinguishes a resize from input:
// it tries to parse every message as JSON and treats one with both fields as a resize. So this
// must be exactly that shape and nothing more.
func (s *serviceWebSocket) Resize(cols, rows uint32) error {
	if cols == 0 || rows == 0 {
		// The far end refuses a dimension below 1 anyway. Sending it would be a message that reads
		// as terminal input if it fails to validate.
		return nil
	}
	payload, err := json.Marshal(struct {
		Cols uint32 `json:"cols"`
		Rows uint32 `json:"rows"`
	}{Cols: cols, Rows: rows})
	if err != nil {
		return err
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.conn.WriteMessage(websocket.TextMessage, payload)
}

// Close hangs up. Called exactly once per session by the registry.
//
// It sends a close frame before dropping the socket, because the far end kills the shell's whole
// process group when the websocket closes: an abrupt drop still gets there, but a clean close is
// what makes the shell exit rather than be killed.
func (s *serviceWebSocket) Close() error {
	s.markClosed()
	s.writeMu.Lock()
	_ = s.conn.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
		time.Now().Add(time.Second),
	)
	s.writeMu.Unlock()
	return s.conn.Close()
}

func (s *serviceWebSocket) markClosed() {
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	s.closed = true
}
