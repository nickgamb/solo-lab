package main

import (
	"net/http"
	"testing"
)

// Every route registers: the mux panics on two patterns it can't order.
func TestAPIRoutes(t *testing.T) {
	apiRoutes(http.NewServeMux(), &Resources{}, &Continuity{}, &Assurance{}, &Relationships{}, &Models{})
}
