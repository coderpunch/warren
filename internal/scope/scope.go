// Package scope finds the sandbox that a request must use.
//
// Open WebUI sets the identity headers on its server (docs/protocol.md,
// section 6.1). A template gives the scope kind. The scope kind and the
// identity together give the context ID. The template ID and the context ID
// together identify one sandbox.
package scope

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"
)

// Kind is the scope of a template.
type Kind string

const (
	KindUser   Kind = "user"
	KindChat   Kind = "chat"
	KindModel  Kind = "model"
	KindShared Kind = "shared"
)

// ValidKind reports whether k is a known scope kind.
func ValidKind(k Kind) bool {
	switch k {
	case KindUser, KindChat, KindModel, KindShared:
		return true
	}
	return false
}

// Header names that Open WebUI sends on an orchestrator connection.
const (
	HeaderUserID     = "X-User-Id"
	HeaderTemplateID = "X-Terminal-Template-Id"
	HeaderChatID     = "X-Terminal-Chat-Id"
	HeaderModelID    = "X-Terminal-Model-Id"
	HeaderSessionID  = "X-Session-Id"
)

// MaxIDLength is the maximum number of bytes in an identity value.
const MaxIDLength = 256

// Errors of this package. The proxy changes them into HTTP status values.
var (
	ErrInvalidHeader = errors.New("invalid identity header")
	ErrMissingUser   = errors.New("user ID is necessary")
	ErrMissingChat   = errors.New("this terminal requires a saved chat")
	ErrMissingModel  = errors.New("this terminal requires a model")
	ErrUnknownKind   = errors.New("unknown scope kind")
)

// Identity is the data that Open WebUI gives for one request.
type Identity struct {
	UserID     string
	TemplateID string
	ChatID     string
	ModelID    string
}

// IdentityFromHeaders reads and examines the identity headers.
// An absent header gives an empty field. A bad header gives an error.
func IdentityFromHeaders(h http.Header) (Identity, error) {
	var id Identity
	for _, f := range []struct {
		name string
		dst  *string
	}{
		{HeaderUserID, &id.UserID},
		{HeaderTemplateID, &id.TemplateID},
		{HeaderChatID, &id.ChatID},
		{HeaderModelID, &id.ModelID},
	} {
		v, err := SingleHeader(h, f.name)
		if err != nil {
			return Identity{}, err
		}
		*f.dst = v
	}
	return id, nil
}

// SingleHeader returns the value of a header that can occur one time only.
// It rejects a value that is too long or that contains control characters.
func SingleHeader(h http.Header, name string) (string, error) {
	values := h.Values(name)
	switch len(values) {
	case 0:
		return "", nil
	case 1:
	default:
		return "", fmt.Errorf("%w: %s occurs %d times", ErrInvalidHeader, name, len(values))
	}
	v := values[0]
	if err := ValidateID(v); err != nil {
		return "", fmt.Errorf("%w: %s: %v", ErrInvalidHeader, name, err)
	}
	return v, nil
}

// ValidateID examines one identity value. An empty value is permitted.
func ValidateID(v string) error {
	if len(v) > MaxIDLength {
		return fmt.Errorf("more than %d bytes", MaxIDLength)
	}
	if !utf8.ValidString(v) {
		return errors.New("not valid UTF-8")
	}
	for _, r := range v {
		if r < 0x20 || r == 0x7f {
			return errors.New("contains a control character")
		}
	}
	if v != strings.TrimSpace(v) {
		return errors.New("starts or ends with white space")
	}
	return nil
}

// Ref identifies one sandbox.
type Ref struct {
	TemplateID string
	Kind       Kind
	// Value is the user ID, chat ID, or model ID. It is empty for the
	// shared scope.
	Value string
}

// ContextID returns the context ID in the form that Open WebUI reads:
// "default", "user:<id>", "chat:<id>", or "model:<id>".
func (r Ref) ContextID() string {
	if r.Kind == KindShared {
		return "default"
	}
	return string(r.Kind) + ":" + r.Value
}

// Name returns a stable name for the Kubernetes objects of the sandbox.
// The name is a DNS label. It contains a digest, thus it does not show the
// user ID, and different sandboxes do not get the same name.
func (r Ref) Name() string {
	h := sha256.New()
	// The length prefix prevents a collision between ("ab","c") and ("a","bc").
	fmt.Fprintf(h, "%d:%s|%s", len(r.TemplateID), r.TemplateID, r.ContextID())
	return "wr-" + hex.EncodeToString(h.Sum(nil))[:40]
}

// nonSavedChatPrefixes are the prefixes of chat IDs that are not saved chats
// (docs/protocol.md, section 6.1). Open WebUI does not verify these IDs.
var nonSavedChatPrefixes = []string{"temporary:", "local:", "channel:"}

// Resolve finds the sandbox for a template scope and an identity.
func Resolve(kind Kind, id Identity) (Ref, error) {
	if id.UserID == "" {
		return Ref{}, ErrMissingUser
	}
	if id.TemplateID == "" {
		return Ref{}, fmt.Errorf("%w: %s is absent", ErrInvalidHeader, HeaderTemplateID)
	}
	ref := Ref{TemplateID: id.TemplateID, Kind: kind}
	switch kind {
	case KindShared:
	case KindUser:
		ref.Value = id.UserID
	case KindChat:
		if id.ChatID == "" {
			return Ref{}, ErrMissingChat
		}
		for _, p := range nonSavedChatPrefixes {
			if strings.HasPrefix(id.ChatID, p) {
				return Ref{}, ErrMissingChat
			}
		}
		ref.Value = id.ChatID
	case KindModel:
		if id.ModelID == "" {
			return Ref{}, ErrMissingModel
		}
		ref.Value = id.ModelID
	default:
		return Ref{}, fmt.Errorf("%w: %q", ErrUnknownKind, kind)
	}
	return ref, nil
}

// ParseContextID changes a context ID back into a scope kind and a value.
func ParseContextID(s string) (Kind, string, error) {
	if s == "default" {
		return KindShared, "", nil
	}
	kind, value, found := strings.Cut(s, ":")
	if !found || value == "" {
		return "", "", fmt.Errorf("invalid context ID %q", s)
	}
	switch Kind(kind) {
	case KindUser, KindChat, KindModel:
		return Kind(kind), value, nil
	}
	return "", "", fmt.Errorf("invalid context ID %q", s)
}
