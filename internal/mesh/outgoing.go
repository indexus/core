package mesh

// OutgoingHandoffs is the number of donor handoff jobs this vnode runs: the
// transfers it is copying out.
func (s *Service) OutgoingHandoffs() int {
	s.jobs.mu.Lock()
	defer s.jobs.mu.Unlock()
	return len(s.jobs.jobs)
}
