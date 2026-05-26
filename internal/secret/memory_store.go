package secret

type MemoryStore struct {
	values map[string]string
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{values: map[string]string{}}
}

func (s *MemoryStore) Set(profileID, key, value string) error {
	s.values[Username(profileID, key)] = value
	return nil
}

func (s *MemoryStore) Get(profileID, key string) (string, bool, error) {
	value, ok := s.values[Username(profileID, key)]
	return value, ok, nil
}

func (s *MemoryStore) Delete(profileID, key string) error {
	delete(s.values, Username(profileID, key))
	return nil
}
