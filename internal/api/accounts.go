package api

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
)

func (a *App) changePassword(w http.ResponseWriter, r *http.Request) {
	u := session(r).User
	if !a.Auth.Allow("account:" + u.ID) {
		fail(w, 429, errors.New("too many account-security changes; try again in 15 minutes"))
		return
	}
	var body struct {
		Current  string `json:"current_password"`
		Password string `json:"new_password"`
	}
	if err := decode(r, &body); err != nil {
		fail(w, 400, err)
		return
	}
	if err := a.Auth.ChangePassword(r.Context(), u.ID, body.Current, body.Password); err != nil {
		fail(w, 400, err)
		return
	}
	a.audit(r.Context(), u.ID, "auth.password_changed", "", "Password changed and all sessions revoked")
	respond(w, 200, map[string]bool{"ok": true})
}
func (a *App) disableTOTP(w http.ResponseWriter, r *http.Request) {
	u := session(r).User
	if !a.Auth.Allow("account:" + u.ID) {
		fail(w, 429, errors.New("too many account-security changes; try again in 15 minutes"))
		return
	}
	var body struct {
		Password string `json:"password"`
		Code     string `json:"code"`
	}
	if err := decode(r, &body); err != nil {
		fail(w, 400, err)
		return
	}
	if err := a.Auth.DisableTOTP(r.Context(), u.ID, body.Password, body.Code); err != nil {
		fail(w, 400, err)
		return
	}
	a.audit(r.Context(), u.ID, "auth.totp_disabled", "", "Two-factor authentication disabled with password and code; all sessions revoked")
	respond(w, 200, map[string]bool{"ok": true})
}
func (a *App) revokeUser(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "userID")
	if err := a.audit(r.Context(), session(r).User.ID, "user.revoke_requested", "", id); err != nil {
		fail(w, 500, errors.New("could not audit the revocation request"))
		return
	}
	if err := a.Auth.RevokeUser(r.Context(), session(r).User.ID, id); err != nil {
		fail(w, 400, err)
		return
	}
	respond(w, 200, map[string]bool{"ok": true})
}
