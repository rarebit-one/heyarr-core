package resources

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/auth"
	"github.com/rarebit-one/heyarr-core/internal/events"
)

// Credentials over the API are admin-only in both directions (see Mount).
//
// The response type is separate from auth.Token rather than reusing it, for the
// reason that matters most about this endpoint: the wire shape must be a thing
// somebody deliberately wrote out field by field, so that a field added to the
// storage struct later cannot appear in an API response by accident.

func tokenFromStore(tk auth.Token) Token {
	scopes := make([]string, 0, len(tk.Scopes))
	for _, s := range auth.Sort(tk.Scopes) {
		scopes = append(scopes, string(s))
	}
	return Token{
		ID:         tk.ID,
		Name:       tk.Name,
		Scopes:     scopes,
		CreatedAt:  tk.CreatedAt.UTC(),
		LastUsedAt: utcPtr(tk.LastUsedAt),
		ExpiresAt:  utcPtr(tk.ExpiresAt),
		RevokedAt:  utcPtr(tk.RevokedAt),
	}
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// listTokens returns every credential.
//
// It is not paginated, and that is a decision rather than an omission: the
// number of tokens on an instance is the number of integrations an operator has
// set up. A cursor here would be ceremony over a list that fits on a screen,
// and unlike works this collection is not written to by a scanner.
func (a *API) listTokens(w http.ResponseWriter, r *http.Request) {
	stored, err := a.tokens.List(r.Context())
	if err != nil {
		a.fail(w, r, "token", err)
		return
	}
	out := make([]Token, 0, len(stored))
	for _, tk := range stored {
		out = append(out, tokenFromStore(tk))
	}
	a.write(w, r, http.StatusOK, page[Token]{Items: out})
}

func (a *API) getToken(w http.ResponseWriter, r *http.Request) {
	tk, err := a.tokens.Get(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		a.fail(w, r, "token", err)
		return
	}
	a.write(w, r, http.StatusOK, tokenFromStore(tk))
}

// createTokenRequest is the POST /tokens body.
type createTokenRequest struct {
	Name   string   `json:"name"`
	Scopes []string `json:"scopes"`
	// ExpiresAt is optional. A token with no expiry is the default because a
	// media server integration that stops working in ninety days is a support
	// ticket, not a security control.
	ExpiresAt *time.Time `json:"expires_at"`
}

func (a *API) createToken(w http.ResponseWriter, r *http.Request) {
	var body createTokenRequest
	if err := decodeJSON(w, r, &body); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	if err := required("name", body.Name); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	if len(body.Scopes) == 0 {
		// Defaulting to `read` would be friendlier and wrong: a caller that
		// forgot the field gets a token, and the one thing nobody checks about
		// a working token is what it can do.
		httpapi.Fail(w, r, problem.BadRequest("scopes is required; it must list at least one of read, write, admin"))
		return
	}
	scopes := make([]auth.Scope, 0, len(body.Scopes))
	for _, raw := range body.Scopes {
		s, err := auth.ParseScope(raw)
		if err != nil {
			httpapi.Fail(w, r, problem.BadRequest(err.Error()))
			return
		}
		scopes = append(scopes, s)
	}
	if body.ExpiresAt != nil && !body.ExpiresAt.After(a.now()) {
		httpapi.Fail(w, r, problem.BadRequest("expires_at is in the past, so the token would be unusable the moment it was minted"))
		return
	}

	created, err := a.tokens.Create(r.Context(), body.Name, scopes, body.ExpiresAt)
	if err != nil {
		a.fail(w, r, "token", err)
		return
	}
	a.emitTokenEvent(r.Context(), events.TypeTokenCreated, created.Token, r)

	w.Header().Set("Location", httpapi.APIPrefix+"/tokens/"+created.Token.ID)
	a.write(w, r, http.StatusCreated, CreatedToken{
		Token:  tokenFromStore(created.Token),
		Secret: created.Secret,
	})
}

// revokeToken is a logical delete: the row stays, revoked_at is set. An audit
// trail that deletes the thing being audited is not one.
func (a *API) revokeToken(w http.ResponseWriter, r *http.Request) {
	tk, err := a.tokens.Revoke(r.Context(), chi.URLParam(r, "id"))
	if errors.Is(err, auth.ErrRevoked) {
		// Revoking twice is not an error worth a 500 and not a success worth a
		// 200: a script must not mistake "already revoked" for "revoked
		// something just now".
		httpapi.Fail(w, r, problem.Conflict("this token was already revoked"))
		return
	}
	if err != nil {
		a.fail(w, r, "token", err)
		return
	}
	a.emitTokenEvent(r.Context(), events.TypeTokenRevoked, tk, r)
	a.write(w, r, http.StatusOK, tokenFromStore(tk))
}

// emitTokenEvent records a credential lifecycle transition. The payload names
// the token and its scopes and never anything that could be replayed as a
// credential — an event log is read by more people than the token was ever
// shown to.
func (a *API) emitTokenEvent(ctx context.Context, eventType string, tk auth.Token, r *http.Request) {
	if _, err := a.events.Emit(ctx, eventType, "token", tk.ID, map[string]any{
		"token_id": tk.ID,
		"name":     tk.Name,
		"scopes":   auth.Join(auth.Sort(tk.Scopes)),
	}); err != nil {
		a.log.Error("a credential changed but its event was not recorded",
			"request_id", httpapi.RequestIDFrom(r.Context()),
			"event", eventType, "token_id", tk.ID, "error", err)
	}
}
