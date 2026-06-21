package backend

import (
	"context"
	pb "github.com/runos-official/nodeagent/l2sec"
	"github.com/runos-official/nodeagent/roslog"
	"time"
)

// AddNodelog sends a node-scoped log entry to Nodeward with the given severity,
// type and message. The structured failure fields (code/cause/remedy/docs_url)
// are left empty; use AddNodelogStructured to populate them.
func AddNodelog(severity int, logType string, message string) error {
	return AddNodelogStructured(severity, logType, message, "", "", "", "")
}

// AddNodelogStructured sends a node-scoped log entry to Nodeward with the given
// severity, type and message plus optional structured failure fields:
//   - code:    a stable machine error code (e.g. NA_APT_LOCK, NA_GENERIC)
//   - cause:   a plain-language cause
//   - remedy:  the user-facing 'Try:' remedy
//   - docsUrl: an optional docs link (may be empty)
//
// All structured fields are optional and back-compatible (empty when not set).
func AddNodelogStructured(severity int, logType, message, code, cause, remedy, docsUrl string) error {
	c, _, backendCancel, conn, err := NodewardL2Sec()
	if err != nil {
		return err
	}
	defer backendCancel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	defer conn.Close()

	request := &pb.AddNodelogRequest{
		Severity: int32(severity),
		Type:     logType,
		Message:  message,
		Code:     code,
		Cause:    cause,
		Remedy:   remedy,
		DocsUrl:  docsUrl,
	}

	roslog.I("Sending AddNodelogRequest", "request", request)

	response, err := c.AddNodelog(ctx, request)
	if err != nil {
		roslog.E("Error add nodelog", err)
		return err
	}

	roslog.I("Received AddNodelog response", "response", response)

	return nil
}
