package srp

// package documentation is in doc.go

/**
 ** Copyright 2017 AgileBits, Inc.
 ** Licensed under the Apache License, Version 2.0 (the "License").
 **/

import (
	"crypto/sha256"
	"fmt"
	"math/big"
)

/*
SRP provides the primary interface to this package.

Your goal is for both your client and server to arrive at the same session key, SRP.Key(),
while proving to each other that they each know their long term secrets (x is the client's
secret and v is the server's secret). Although the key that you arrive at is 32 bytes, its
strength is a function of the group size used.

Creating the SRP object with with NewSRPServer()/NewSRPClient() takes care of generating your ephemeral
secret (a or b depending on whether you are a client or server), your public
ephemeral key (A or B depending on whether you are a client or server),
the multiplier k. (There is a setter for k if you wish to use a different scheme
to set those.

A typical use by a server might be something like

	server := NewSRPServer(KnownGroups[RFC5054Group4096], v, nil)

	A := getAfromYourClientConnection(...) // your code
	if result, err := server.SetOthersPublic(A); result == nil || err != nil {
		// client sent a malicious A. Kill this session now
	}

	sendBtoClientSomehow(server.EphemeralPublic())

	if sessionKey, err := server.Key(); sessionKey == nil || err != nil {
		// something went wrong
	}

	// You must still prove that both server and client created the same Key.

This still leaves some work outside of what the SRP object provides:

1. The key derivation of x is not handled by this object.

2. The communication between client and server is not handled by this object.
*/
type SRP struct {
	group            *Group
	ephemeralPrivate *big.Int // Little a or little b (ephemeral secrets)
	ephemeralPublicA *big.Int // Public A
	ephemeralPublicB *big.Int // Public A and B ephemeral values
	x, v             *big.Int // x and verifier (long term secrets)
	u                *big.Int // calculated scrambling parameter
	k                *big.Int // multiplier parameter
	premasterKey     *big.Int // unhashed derived session secret
	key              []byte   // H(preMasterSecret)
	isServer         bool
	badState         bool
	isServerProved   bool   // whether server has proved knowledge of key
	m                []byte // M is server proof knowledge of key
	cProof           []byte // Client proof of knowledge of key
}

// bigZero is a BigInt zero
var bigZero = big.NewInt(0)
var bigOne = big.NewInt(1)

/*
NewSRPClient sets up an SRP object for a client.

group *Group: Pointer to the Diffie-Hellman group to be used.

x *big.Int: Your long term secret, x.

k *big.Int: If you wish to manually set the multiplier, little k, pass in
a non-nil bigInt. If you set this to nil, then we will generate one for you.
You need the same k on both server and client.
*/
func NewSRPClient(group *Group, x *big.Int, k *big.Int) *SRP {
	return newSRP(false, group, x, k)
}

/*
NewSRPServer sets up an SRP object for a server.

group *Group: Pointer to the Diffie-Hellman group to be used.

v *big.Int: Your long term secret, v.

k *big.Int: If you wish to manually set the multiplier, little k, pass in
a non-nil bigInt. If you set this to nil, then we will generate one for you.
You need the same k on both server and client.
*/
func NewSRPServer(group *Group, v *big.Int, k *big.Int) *SRP {
	return newSRP(true, group, v, k)
}

func newSRP(serverSide bool, group *Group, xORv *big.Int, k *big.Int) *SRP {
	s := &SRP{
		// Setting these to Int-zero gives me a useful way to test
		// if these have been properly set later
		ephemeralPublicA: big.NewInt(0),
		ephemeralPrivate: big.NewInt(0),
		ephemeralPublicB: big.NewInt(0),
		u:                big.NewInt(0),
		k:                big.NewInt(0),
		x:                big.NewInt(0),
		v:                big.NewInt(0),
		premasterKey:     big.NewInt(0),
		key:              nil,
		group:            group,

		badState: false,
		isServer: serverSide,

		m:              nil,
		cProof:         nil,
		isServerProved: false,
	}

	if s.isServer {
		s.v.Set(xORv)
	} else {
		s.x.Set(xORv)
	}

	if k != nil {
		// should probably do some sanity checks on k here
		s.k.Set(k)
	} else {
		s.makeLittleK()
	}
	s.generateMySecret()
	if s.isServer {
		s.makeB()
	} else {
		s.makeA()
	}
	return s
}

// EphemeralPublic returns A on client or B on server
// If you are a client, you will need to send A to the server.
// If you are a server, you will need to send B to the client.
// But this abstracts away from user needing to keep A and B straight. Caller
// just needs to send EphemeralPublic() to the other party.
func (s *SRP) EphemeralPublic() *big.Int {
	if s.isServer {
		if s.ephemeralPublicB.Cmp(bigZero) == 0 {
			s.makeB()
		}
		return s.ephemeralPublicB
	}
	if s.ephemeralPublicA.Cmp(bigZero) == 0 {
		s.makeA()
	}
	return s.ephemeralPublicA
}

// IsPublicValid checks to see whether public A or B is valid within the group
// A client can do very bad things by sending a malicious A to the server.
// The server can do mildly bad things by sending a malicious B to the client.
// This method is public in case the user wishes to check those values earlier than
// than using SetOthersPublic(), which also performs this check.
func (s *SRP) IsPublicValid(AorB *big.Int) bool {

	result := big.Int{}
	// There are three ways to fail.
	// 1. If we aren't checking with respect to a valid group
	// 2. If public paramater zero or a multiple of M
	// 3. If public parameter is not relatively prime to N (a bad group?)
	if s.group == nil {
		return false
	}
	if s.group.g.Cmp(bigZero) == 0 {
		return false
	}

	if result.Mod(AorB, s.group.n); result.Sign() == 0 {
		return false
	}

	if result.GCD(nil, nil, AorB, s.group.n).Cmp(bigOne) != 0 {
		return false
	}
	return true
}

// Verifier retruns the verifier as calculated by the client.
// On first enrollment, the client will need to send the verifier to the server,
// which the server will store as its long term secret. Only a client can
// compute the verifier as it requires knowledge of x.
func (s *SRP) Verifier() (*big.Int, error) {
	if s.isServer {
		return nil, fmt.Errorf("server may not produce a verifier")
	}
	return s.makeVerifier()
}

// SetOthersPublic sets A if server and B if client
// Caller *MUST* check for error status and abort the session
// on error. This setter will invoke IsPublicValid() and error
// status must be heeded, as other party may attempt to send
// a malicious ephemeral public key (A or B).
//
// When used by the server, this sets A, when used by the client
// it sets B. But caller doesn't need to worry about whether this
// is A or B. Instead the caller just needs to know that they
// are setting the public ephemeral key received from the other party.
func (s *SRP) SetOthersPublic(AorB *big.Int) error {
	if !s.IsPublicValid(AorB) {
		s.badState = true
		s.key = nil
		return fmt.Errorf("invalid public exponent")
	}

	if s.isServer {
		s.ephemeralPublicA.Set(AorB)
	} else {
		s.ephemeralPublicB.Set(AorB)
	}
	return nil
}

// Key creates and returns the session Key
//
// Caller MUST check error status.
//
// Once the ephemeral public key is received from the other party and properly
// set, SRP should have enough information to compute the session key.
//
// If and only if, each party knowns their respective long term secret
// (x for client, v for server) will both parties compute the same Key.
// Be sure to confirm that client and server have the same key before
// using it.
//
// Note that although the resulting key is 256 bits, its effective strength
// is (typically) far less and depends on the group used.
// 8 * (SRP.Group.ExponentSize / 2) should provide a reasonable estimate if you
// need that.
func (s *SRP) Key() ([]byte, error) {
	if s.key != nil {
		return s.key, nil
	}
	if s.badState {
		return nil, fmt.Errorf("we've got bad data")
	}
	if s.group == nil {
		return nil, fmt.Errorf("group not set")
	}
	// This test is so I'm not lying to gosec wrt to G105
	if s.group.n.Cmp(bigZero) == 0 {
		return nil, fmt.Errorf("group has 0 modulus")
	}
	// Because of tests, we don't want to always recalculate u
	if !s.isUValid() {
		if u, err := s.calculateU(); u == nil || err != nil {
			return nil, fmt.Errorf("failed to calculate u: %s", err)
		}
	}
	// We must refuse to calculate Key when u == 0
	if !s.isUValid() {
		s.badState = true
		return nil, fmt.Errorf("invalid u")
	}
	if s.ephemeralPrivate.Cmp(bigZero) == 0 {
		return nil, fmt.Errorf("cannot make Key with my ephemeral secret")
	}

	b := &big.Int{} // base
	e := &big.Int{} // exponent

	if s.isServer {
		// S = (Av^u) ^ b
		if s.v == nil || s.ephemeralPublicA == nil {
			return nil, fmt.Errorf("not enough is known to create Key")
		}
		b.Exp(s.v, s.u, s.group.n) // #nosec G105
		b.Mul(b, s.ephemeralPublicA)
		e = s.ephemeralPrivate
	} else { // client
		// (B - kg^x) ^ (a + ux)
		if s.ephemeralPublicB == nil || s.k == nil || s.x == nil {
			return nil, fmt.Errorf("not enough is known to create Key")
		}
		e.Mul(s.u, s.x)
		e.Add(e, s.ephemeralPrivate)

		b.Exp(s.group.g, s.x, s.group.n) // #nosec G105
		b.Mul(b, s.k)
		b.Sub(s.ephemeralPublicB, b)
		b.Mod(b, s.group.n)
	}

	s.premasterKey.Exp(b, e, s.group.n)

	h := sha256.New()
	if _, err := h.Write(s.premasterKey.Bytes()); err != nil {
		return nil, fmt.Errorf("failed to write premasterKey to hasher: %v", err)
	}

	s.key = h.Sum(nil)

	if len(s.key) != h.Size() {
		return nil, fmt.Errorf("key size should be %d, but instead is %d", h.Size(), len(s.key))
	}

	return s.key, nil
}
