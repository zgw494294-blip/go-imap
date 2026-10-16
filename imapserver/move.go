package imapserver

import (
	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/internal/imapwire"
)

func (c *Conn) handleMove(dec *imapwire.Decoder, numKind NumKind) error {
	numSet, dest, err := readCopy(numKind, dec)
	if err != nil {
		return err
	}
	if numKind == NumKindSeq && c.uidOnlyEnabled() {
		return errUIDRequired
	}
	if err := c.checkState(imap.ConnStateSelected); err != nil {
		return err
	}
	session, ok := c.session.(SessionMove)
	if !ok {
		return newClientBugError("MOVE is not supported")
	}
	w := &MoveWriter{conn: c}
	return session.Move(w, numSet, dest)
}

// MoveWriter writes responses for the MOVE command.
//
// Servers must first call WriteCopyData once, then call WriteExpunge (or
// WriteExpungeUID in UIDONLY mode) any number of times.
type MoveWriter struct {
	conn *Conn
}

// UIDOnly reports whether the connection has UIDONLY enabled (RFC 9586).
func (w *MoveWriter) UIDOnly() bool {
	return w.conn.uidOnlyEnabled()
}

// WriteCopyData writes the untagged COPYUID response for a MOVE command.
func (w *MoveWriter) WriteCopyData(data *imap.CopyData) error {
	return w.conn.writeCopyOK("", data)
}

// WriteExpunge writes an EXPUNGE response for a MOVE command.
func (w *MoveWriter) WriteExpunge(seqNum uint32) error {
	return w.conn.writeExpunge(seqNum)
}

// WriteExpungeUID writes a VANISHED response for a MOVE command (RFC 9586).
//
// It must be used instead of WriteExpunge when UIDONLY is enabled on the
// connection.
func (w *MoveWriter) WriteExpungeUID(uid imap.UID) error {
	if uid == 0 {
		panic("imapserver: MoveWriter.WriteExpungeUID requires a non-zero UID")
	}
	return w.conn.writeVanished(imap.UIDSetNum(uid))
}
