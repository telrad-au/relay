package main

import (
	"errors"

	"github.com/telrad-au/relay/internal/retrieval"
)

// An unsafe or locally unapproved scope withholds authorization, not transport.
// Configuration, key, authentication and network failures remain errors.
func authorizationScopeRejected(err error) bool {
	return errors.Is(err, retrieval.ErrPolicy) || errors.Is(err, retrieval.ErrPermit) || errors.Is(err, retrieval.ErrIdentity)
}
