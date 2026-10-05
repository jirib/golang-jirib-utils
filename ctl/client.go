package ctl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"time"
)

// DefaultTimeout bounds one request/reply exchange when no timeout is specified.
const DefaultTimeout = 10 * time.Minute

// ErrNoDaemon indicates the socket exists but no process is listening.
var ErrNoDaemon = errors.New("ctl: no daemon is listening on the control socket")

// Client is a control-socket client communicating over a Unix domain socket.
type Client struct {
	// Path is the socket path to dial. If empty, derived via DefaultSocketPath(App).
	Path string
	// App is the application name used to resolve the default socket path when Path is empty.
	App string
	// Timeout bounds an exchange. Zero uses DefaultTimeout; negative disables the client deadline.
	Timeout time.Duration

	// Dial overrides the dialer (useful in tests).
	Dial func(ctx context.Context, network, address string) (net.Conn, error)
	// Log receives debug log lines.
	Log Logger
}

// Logger is the subset of *slog.Logger Client uses.
type Logger interface {
	Debug(msg string, args ...any)
}

func (c *Client) logger() Logger {
	if c.Log != nil {
		return c.Log
	}
	return nopLogger{}
}

type nopLogger struct{}

func (nopLogger) Debug(string, ...any) {}

func (c *Client) socketPath() (string, error) {
	if c.Path != "" {
		return c.Path, nil
	}
	if c.App == "" {
		return "", errors.New("ctl: client needs either Path or App")
	}
	return DefaultSocketPath(c.App)
}

// CallRaw sends one request and returns the unparsed reply payload.
func (c *Client) CallRaw(ctx context.Context, typ Type, request any) ([]byte, error) {
	path, err := c.socketPath()
	if err != nil {
		return nil, err
	}
	timeout := c.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	log := c.logger()
	var payload []byte
	if request != nil {
		if payload, err = json.Marshal(request); err != nil {
			return nil, fmt.Errorf("ctl: encoding request: %w", err)
		}
	}

	dial := c.Dial
	if dial == nil {
		dial = func(ctx context.Context, network, address string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, address)
		}
	}
	start := time.Now()
	conn, err := dial(ctx, "unix", path)
	if err != nil {
		return nil, dialError(path, err)
	}
	defer conn.Close()

	stop := context.AfterFunc(ctx, func() {
		conn.Close()
	})
	defer stop()

	log.Debug("control request", "path", path, "type", typ.String(), "bytes", len(payload))

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if err := WriteMessage(conn, typ, payload); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("ctl: sending request: %w", ctxErr)
		}
		return nil, fmt.Errorf("ctl: sending request: %w", err)
	}

	replyType, body, err := ReadMessage(conn)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("ctl: %s did not reply within %v: %w", path, timeout, context.DeadlineExceeded)
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("ctl: %s: %w", path, ctxErr)
		}
		return nil, fmt.Errorf("ctl: reading reply: %w", err)
	}
	log.Debug("control reply", "path", path, "type", replyType.String(),
		"bytes", len(body), "duration", time.Since(start))

	switch replyType {
	case TypeError:
		return nil, replyError(body)
	case TypeReply:
		return body, nil
	default:
		return nil, fmt.Errorf("%w: daemon replied with %s", ErrProtocol, replyType)
	}
}

// Call sends one request, marshaling request as JSON, and unmarshals the JSON reply into result.
func (c *Client) Call(ctx context.Context, typ Type, request, result any) error {
	body, err := c.CallRaw(ctx, typ, request)
	if err != nil {
		return err
	}
	if result == nil || len(body) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, result); err != nil {
		return fmt.Errorf("ctl: decoding reply: %w", err)
	}
	return nil
}

type errorBody struct {
	Error string `json:"error"`
}

func replyError(body []byte) error {
	var e errorBody
	if err := json.Unmarshal(body, &e); err != nil || e.Error == "" {
		return fmt.Errorf("ctl: daemon reported an error with an unreadable body: %q", string(body))
	}
	return errors.New(e.Error)
}

func dialError(path string, err error) error {
	if _, statErr := os.Stat(path); statErr != nil {
		return fmt.Errorf("%w: %s", ErrNoDaemon, path)
	}
	return fmt.Errorf("ctl: cannot connect to %s: %w (socket exists but refused connection)", path, err)
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}
