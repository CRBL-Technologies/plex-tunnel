package relay

import "sync"

type Router struct {
	mu     sync.RWMutex
	agents map[string]*AgentSession
}

func NewRouter() *Router {
	return &Router{agents: make(map[string]*AgentSession)}
}

func (r *Router) Set(subdomain string, session *AgentSession) (previous *AgentSession) {
	r.mu.Lock()
	defer r.mu.Unlock()

	previous = r.agents[subdomain]
	r.agents[subdomain] = session
	return previous
}

func (r *Router) Get(subdomain string) (*AgentSession, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	session, ok := r.agents[subdomain]
	return session, ok
}

func (r *Router) Delete(subdomain string, session *AgentSession) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if current, ok := r.agents[subdomain]; ok && current == session {
		delete(r.agents, subdomain)
	}
}

func (r *Router) Snapshot() []*AgentSession {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]*AgentSession, 0, len(r.agents))
	for _, session := range r.agents {
		out = append(out, session)
	}
	return out
}
