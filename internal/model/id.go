package model

import (
	"crypto/rand"
	"database/sql/driver"
	"encoding/hex"
	"errors"
	"fmt"
)

// ID is a 128-bit identifier stored as BINARY(16) and rendered as a canonical
// UUID. Binary rather than CHAR(36) because these are the primary keys of the
// hottest tables in the system.
type ID [16]byte

var ErrInvalidID = errors.New("invalid identifier")

func NewID() (ID, error) {
	var id ID
	if _, err := rand.Read(id[:]); err != nil {
		return ID{}, fmt.Errorf("generate id: %w", err)
	}
	id[6] = (id[6] & 0x0f) | 0x40 // version 4
	id[8] = (id[8] & 0x3f) | 0x80 // variant 10
	return id, nil
}

func MustNewID() ID {
	id, err := NewID()
	if err != nil {
		panic(err)
	}
	return id
}

func (id ID) IsZero() bool { return id == ID{} }

func (id ID) Bytes() []byte { b := make([]byte, 16); copy(b, id[:]); return b }

func (id ID) String() string {
	var buf [36]byte
	hex.Encode(buf[0:8], id[0:4])
	buf[8] = '-'
	hex.Encode(buf[9:13], id[4:6])
	buf[13] = '-'
	hex.Encode(buf[14:18], id[6:8])
	buf[18] = '-'
	hex.Encode(buf[19:23], id[8:10])
	buf[23] = '-'
	hex.Encode(buf[24:36], id[10:16])
	return string(buf[:])
}

func ParseID(s string) (ID, error) {
	var clean []byte
	for i := 0; i < len(s); i++ {
		if s[i] != '-' {
			clean = append(clean, s[i])
		}
	}
	if len(clean) != 32 {
		return ID{}, ErrInvalidID
	}
	var id ID
	if _, err := hex.Decode(id[:], clean); err != nil {
		return ID{}, ErrInvalidID
	}
	return id, nil
}

func (id ID) MarshalText() ([]byte, error) { return []byte(id.String()), nil }

func (id *ID) UnmarshalText(b []byte) error {
	parsed, err := ParseID(string(b))
	if err != nil {
		return err
	}
	*id = parsed
	return nil
}

func (id ID) Value() (driver.Value, error) { return id.Bytes(), nil }

func (id *ID) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*id = ID{}
		return nil
	case []byte:
		if len(v) == 16 {
			copy(id[:], v)
			return nil
		}
		parsed, err := ParseID(string(v))
		if err != nil {
			return err
		}
		*id = parsed
		return nil
	case string:
		parsed, err := ParseID(v)
		if err != nil {
			return err
		}
		*id = parsed
		return nil
	default:
		return fmt.Errorf("cannot scan %T into ID", src)
	}
}
