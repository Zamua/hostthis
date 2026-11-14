package ssh

import (
	"errors"
	"io"

	"github.com/charmbracelet/ssh"
	gossh "golang.org/x/crypto/ssh"
)

// errConnectionLost is what an upload body reports instead of EOF when the
// connection ended under it: the bytes read so far are a truncated body.
var errConnectionLost = errors.New("connection lost before the upload finished")

// confirmedEOFReader reports io.EOF only after the end of the body is
// confirmed to be the client's own end of input. A channel also reads as EOF
// when the whole connection drops mid-upload, and saving what arrived by then
// would publish a truncated paste. The check runs once, at EOF.
type confirmedEOFReader struct {
	r       io.Reader
	alive   func() bool
	checked bool
	lost    bool
}

func (c *confirmedEOFReader) Read(p []byte) (int, error) {
	if c.lost {
		return 0, errConnectionLost
	}
	n, err := c.r.Read(p)
	if errors.Is(err, io.EOF) && !c.checked {
		c.checked = true
		if !c.alive() {
			c.lost = true
			return n, errConnectionLost
		}
	}
	return n, err
}

// uploadBody wraps the session's input so a dropped connection fails the
// upload instead of ending its body early.
func uploadBody(sess ssh.Session) io.Reader {
	return &confirmedEOFReader{r: sess, alive: func() bool { return connAlive(sess) }}
}

// connAlive asks the client for a round trip. A client that sent its end of
// input keeps the connection open to read the reply, so it answers (a refusal
// of an unknown request is an answer). Once the connection is gone the
// request fails at once: the transport closes its reply queue when it stops.
func connAlive(sess ssh.Session) bool {
	if sess.Context().Err() != nil {
		return false
	}
	conn, ok := sess.Context().Value(ssh.ContextKeyConn).(gossh.Conn)
	if !ok {
		return true
	}
	_, _, err := conn.SendRequest("keepalive@openssh.com", true, nil)
	return err == nil
}
