package records

import "errors"

var ErrNotFound = errors.New("record not found")

type Store struct {
	values map[string]string
}

func NewStore() *Store { return &Store{values: make(map[string]string)} }

func (s *Store) Put(key, value string) { s.values[key] = value }

func (s *Store) Get(key string) (string, error) {
	value, ok := s.values[key]
	if !ok {
		return "", ErrNotFound
	}
	return value, nil
}
