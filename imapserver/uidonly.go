package imapserver

import (
	"github.com/emersion/go-imap/v2"
)

// errUIDRequired is returned when a client uses a message sequence number
// while UIDONLY is enabled (RFC 9586).
var errUIDRequired = &imap.Error{
	Type: imap.StatusResponseTypeBad,
	Code: imap.ResponseCodeUIDRequired,
	Text: "Message sequence numbers are not allowed once UIDONLY is enabled",
}

// uidOnlyEnabled reports whether the client has enabled the UIDONLY extension
// on this connection (RFC 9586).
//
// The state is per-connection: it persists across mailbox selection changes
// and is reset by UNAUTHENTICATE.
func (c *Conn) uidOnlyEnabled() bool {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.enabled.Has(imap.CapUIDOnly)
}

// UIDOnlyEnabled reports whether the client has enabled the UIDONLY extension
// on this connection (RFC 9586).
//
// Backends can use this to determine which response writer methods to call.
func (c *Conn) UIDOnlyEnabled() bool {
	return c.uidOnlyEnabled()
}

// writeVanished writes a VANISHED response (RFC 7162, without the EARLIER
// tag), as used by UIDONLY in place of EXPUNGE.
func (c *Conn) writeVanished(uids imap.UIDSet) error {
	enc := newResponseEncoder(c)
	defer enc.end()
	enc.Atom("*").SP().Atom("VANISHED").SP().NumSet(uids)
	return enc.CRLF()
}
