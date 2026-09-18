package records

type Service struct {
	store *Store
}

func NewService(store *Store) *Service { return &Service{store: store} }

func (s *Service) Lookup(key string) (string, error) { return s.store.Get(key) }
