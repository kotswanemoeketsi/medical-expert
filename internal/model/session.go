package model

import "time"

// Session is a signed-in browser or device. Only the SHA-256 of the bearer
// token is stored; the token itself exists only in the client.
type Session struct {
	ID             ID
	TenantID       ID
	UserID         ID
	TokenHash      [32]byte
	MFAPending     bool // password accepted, second factor outstanding
	IssuedAt       time.Time
	LastSeenAt     time.Time
	AbsoluteExpiry time.Time
	IP             string
	UserAgent      string
}
