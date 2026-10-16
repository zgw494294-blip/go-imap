package imapserver

import (
	"strings"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/internal"
	"github.com/emersion/go-imap/v2/internal/imapwire"
)

func (c *Conn) handleCreate(tag string, dec *imapwire.Decoder) error {
	var (
		name    string
		options imap.CreateOptions
	)
	if !dec.ExpectSP() || !dec.ExpectMailbox(&name) {
		return dec.Err()
	}
	if dec.SP() {
		var name string
		if !dec.ExpectSpecial('(') || !dec.ExpectAtom(&name) || !dec.ExpectSP() {
			return dec.Err()
		}
		switch strings.ToUpper(name) {
		case "USE":
			var err error
			options.SpecialUse, err = internal.ExpectMailboxAttrList(dec)
			if err != nil {
				return err
			}
		default:
			return newClientBugError("unknown CREATE parameter")
		}
		if !dec.ExpectSpecial(')') {
			return dec.Err()
		}
	}
	if !dec.ExpectCRLF() {
		return dec.Err()
	}
	if err := c.checkState(imap.ConnStateAuthenticated); err != nil {
		return err
	}

	if objectIDSession, ok := c.session.(SessionObjectID); ok && c.server.options.caps().Has(imap.CapObjectID) {
		data, err := objectIDSession.CreateMailbox(name, &options)
		if err != nil {
			return err
		}
		if data == nil || !validObjectID(data.MailboxID) {
			return &imap.Error{
				Type: imap.StatusResponseTypeNo,
				Code: imap.ResponseCodeServerBug,
				Text: "Internal server error",
			}
		}
		if err := c.poll("CREATE"); err != nil {
			return err
		}
		return c.writeCreateOK(tag, data)
	}

	if err := c.session.Create(name, &options); err != nil {
		return err
	}
	if err := c.poll("CREATE"); err != nil {
		return err
	}
	return c.writeStatusResp(tag, &imap.StatusResponse{
		Type: imap.StatusResponseTypeOK,
		Text: "CREATE completed",
	})
}

func (c *Conn) writeCreateOK(tag string, data *imap.CreateData) error {
	enc := newResponseEncoder(c)
	defer enc.end()

	enc.Atom(tag).SP().Atom("OK").SP()
	enc.Special('[').Atom(string(imap.ResponseCodeMailboxID)).SP()
	writeObjectID(enc.Encoder, data.MailboxID)
	enc.Special(']').SP()
	enc.Text("CREATE completed")
	return enc.CRLF()
}

// validObjectID checks whether s is a valid RFC 8474 objectid: a non-empty
// sequence of at most 255 characters from ALPHA / DIGIT / "_" / "-".
func validObjectID(s string) bool {
	if len(s) < 1 || len(s) > 255 {
		return false
	}
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch >= 'a' && ch <= 'z':
		case ch >= 'A' && ch <= 'Z':
		case ch >= '0' && ch <= '9':
		case ch == '_' || ch == '-':
		default:
			return false
		}
	}
	return true
}

// writeObjectID writes a parenthesized objectid, e.g. "(M0001)".
//
// The caller is responsible for validating the identifier beforehand.
func writeObjectID(enc *imapwire.Encoder, id string) {
	enc.Special('(').Atom(id).Special(')')
}
