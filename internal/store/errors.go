package store

import "errors"

var (
	ErrNotFound        = errors.New("not found")
	ErrVersionConflict = errors.New("version conflict")
	ErrAlreadyExists   = errors.New("already exists")
)
