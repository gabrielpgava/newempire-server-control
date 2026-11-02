package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gabrielpgava/newempire-server-control/internal/database"
	"github.com/gabrielpgava/newempire-server-control/internal/model"
	"github.com/gabrielpgava/newempire-server-control/internal/secrets"
	"golang.org/x/crypto/argon2"
)

type User struct {
	ID        string `json:"id"`
	Username  string `json:"username"`
	Role      string `json:"role"`
	TwoFactor bool   `json:"two_factor"`
}
type Session struct {
	User  User   `json:"user"`
	CSRF  string `json:"csrf_token"`
	Token string `json:"-"`
}
type Service struct {
	DB        *database.Store
	Vault     *secrets.Vault
	hashLimit chan struct{}
	mu        sync.Mutex
	limits    map[string]attempt
}
type attempt struct {
	Count int
	Until time.Time
}

func New(db *database.Store, vault *secrets.Vault) *Service {
	return &Service{DB: db, Vault: vault, hashLimit: make(chan struct{}, 2), limits: map[string]attempt{}}
}

func Token() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
func HashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

func PasswordHash(password string) (string, error) {
	if len(password) < 12 || len(password) > 1024 {
		return "", errors.New("password must contain 12 to 1024 bytes")
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	h := argon2.IDKey([]byte(password), salt, 3, 64*1024, 2, 32)
	return "$argon2id$v=19$m=65536,t=3,p=2$" + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(h), nil
}

func VerifyPassword(encoded, password string) bool {
	if len(password) > 1024 {
		return false
	}
	p := strings.Split(encoded, "$")
	if len(p) != 6 || p[1] != "argon2id" || p[2] != "v=19" || p[3] != "m=65536,t=3,p=2" {
		return false
	}
	salt, e1 := base64.RawStdEncoding.DecodeString(p[4])
	want, e2 := base64.RawStdEncoding.DecodeString(p[5])
	if e1 != nil || e2 != nil || len(salt) != 16 || len(want) != 32 {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, 3, 64*1024, 2, 32)
	return subtle.ConstantTimeCompare(got, want) == 1
}

func (s *Service) Required(ctx context.Context) (bool, error) {
	var count int
	err := s.DB.QueryRowContext(ctx, "SELECT count(*) FROM users").Scan(&count)
	return count == 0, err
}

func validUser(username, role string) error {
	if len(username) < 3 || len(username) > 64 || strings.TrimSpace(username) != username {
		return errors.New("username must contain 3 to 64 characters without surrounding spaces")
	}
	for _, r := range username {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.') {
			return errors.New("username contains unsupported characters")
		}
	}
	if role != "owner" && role != "admin" && role != "operator" && role != "viewer" {
		return errors.New("invalid user role")
	}
	return nil
}

func (s *Service) CreateUser(ctx context.Context, username, password, role string, bootstrap bool) (User, error) {
	if err := validUser(username, role); err != nil {
		return User{}, err
	}
	select {
	case s.hashLimit <- struct{}{}:
		defer func() { <-s.hashLimit }()
	case <-ctx.Done():
		return User{}, ctx.Err()
	}
	hash, err := PasswordHash(password)
	if err != nil {
		return User{}, err
	}
	u := User{ID: model.NewID(), Username: username, Role: role}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return u, err
	}
	defer tx.Rollback()
	if bootstrap {
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM users").Scan(&count); err != nil {
			return u, err
		}
		if count != 0 {
			return u, errors.New("setup has already completed")
		}
		if role != "owner" {
			return u, errors.New("first administrator must be owner")
		}
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO users(id,username,password_hash,role,created_at) VALUES(?,?,?,?,?)", u.ID, u.Username, hash, u.Role, time.Now().Unix())
	if err != nil {
		return u, errors.New("could not create user; username may already exist")
	}
	return u, tx.Commit()
}

func (s *Service) Allow(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if len(s.limits) > 4096 {
		for k, a := range s.limits {
			if now.After(a.Until) {
				delete(s.limits, k)
			}
		}
		if len(s.limits) > 4096 {
			return false
		}
	}
	a := s.limits[key]
	if now.After(a.Until) {
		a = attempt{Until: now.Add(15 * time.Minute)}
	}
	a.Count++
	s.limits[key] = a
	return a.Count <= 10
}

func (s *Service) Login(ctx context.Context, username, password, code string) (Session, error) {
	select {
	case s.hashLimit <- struct{}{}:
		defer func() { <-s.hashLimit }()
	case <-ctx.Done():
		return Session{}, ctx.Err()
	}
	var u User
	var hash string
	var encrypted []byte
	err := s.DB.QueryRowContext(ctx, "SELECT id,username,role,password_hash,totp FROM users WHERE username=?", username).Scan(&u.ID, &u.Username, &u.Role, &hash, &encrypted)
	if errors.Is(err, sql.ErrNoRows) {
		VerifyPassword("$argon2id$v=19$m=65536,t=3,p=2$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", password)
		return Session{}, errors.New("invalid credentials or authentication code")
	}
	if err != nil {
		return Session{}, err
	}
	if !VerifyPassword(hash, password) {
		return Session{}, errors.New("invalid credentials or authentication code")
	}
	if len(encrypted) > 0 {
		secret, err := s.Vault.Open(encrypted, "totp:"+u.ID)
		if err != nil || !VerifyTOTP(string(secret), code, time.Now()) {
			return Session{}, errors.New("invalid credentials or authentication code")
		}
		u.TwoFactor = true
	}
	sess := Session{User: u, CSRF: Token(), Token: Token()}
	_, err = s.DB.ExecContext(ctx, "INSERT INTO sessions(token_hash,user_id,csrf,expires_at) VALUES(?,?,?,?)", HashToken(sess.Token), u.ID, sess.CSRF, time.Now().Add(12*time.Hour).Unix())
	return sess, err
}

func (s *Service) Session(ctx context.Context, token string) (Session, error) {
	if len(token) != 43 {
		return Session{}, errors.New("authentication required")
	}
	var sess Session
	var two bool
	err := s.DB.QueryRowContext(ctx, "SELECT u.id,u.username,u.role,u.totp IS NOT NULL,s.csrf FROM sessions s JOIN users u ON u.id=s.user_id WHERE s.token_hash=? AND s.expires_at>?", HashToken(token), time.Now().Unix()).Scan(&sess.User.ID, &sess.User.Username, &sess.User.Role, &two, &sess.CSRF)
	sess.User.TwoFactor = two
	return sess, err
}

func (s *Service) Logout(ctx context.Context, token string) error {
	_, err := s.DB.ExecContext(ctx, "DELETE FROM sessions WHERE token_hash=?", HashToken(token))
	return err
}

func (s *Service) Users(ctx context.Context) ([]User, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT id,username,role,totp IS NOT NULL FROM users ORDER BY username")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	users := []User{}
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Username, &u.Role, &u.TwoFactor); err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

func VerifyTOTP(secret, code string, now time.Time) bool {
	if len(code) != 6 {
		return false
	}
	if _, err := strconv.Atoi(code); err != nil {
		return false
	}
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(secret))
	if err != nil || len(key) < 20 {
		return false
	}
	for delta := -1; delta <= 1; delta++ {
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(now.Unix()/30+int64(delta)))
		h := hmac.New(sha1.New, key)
		h.Write(b[:])
		sum := h.Sum(nil)
		offset := sum[len(sum)-1] & 15
		n := (binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff) % 1000000
		if subtle.ConstantTimeCompare([]byte(fmt.Sprintf("%06d", n)), []byte(code)) == 1 {
			return true
		}
	}
	return false
}

func NewTOTP() string {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)
}

func (s *Service) EnableTOTP(ctx context.Context, userID, password, secret, code string) error {
	var hash string
	if err := s.DB.QueryRowContext(ctx, "SELECT password_hash FROM users WHERE id=?", userID).Scan(&hash); err != nil {
		return err
	}
	if !VerifyPassword(hash, password) || !VerifyTOTP(secret, code, time.Now()) {
		return errors.New("invalid password or authentication code")
	}
	encrypted, err := s.Vault.Seal([]byte(secret), "totp:"+userID)
	if err != nil {
		return err
	}
	_, err = s.DB.ExecContext(ctx, "UPDATE users SET totp=? WHERE id=?", encrypted, userID)
	if err == nil {
		_, err = s.DB.ExecContext(ctx, "DELETE FROM sessions WHERE user_id=?", userID)
	}
	return err
}

func Permitted(role, level string) bool {
	ranks := map[string]int{"viewer": 1, "operator": 2, "admin": 3, "owner": 4}
	return ranks[role] >= ranks[level] && ranks[level] > 0
}
