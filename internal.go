package srp

import (
	rand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"
)

/*
The principle srp.go file was getting too long, so I'm putting the non-exported
methods in here.
*/

// generateMySecret creates the little a or b
// According to RFC 5054, this should be at least 32 bytes
// According to RFC 2631 this should be uniform in the range
// [2, q-2], where q is the Sophie Germain prime from which
// N was created.
// According to RFC 3526 §8 there are some specific sizes depending
// on the group. We go with RFC 3526 values if available, otherwise
// a minimum of 32 bytes.

func (s *SRP) generateMySecret() *big.Int {

	eSize := maxInt(s.group.ExponentSize, MinExponentSize)
	bytes := make([]byte, eSize)
	_, err := rand.Read(bytes)
	if err != nil {
		// If we can't get random bytes from the system, then we have no business doing anything crypto related.
		panic(fmt.Sprintf("Failed to get random bytes: %v", err))
	}
	ephemeralPrivate := &big.Int{}
	ephemeralPrivate.SetBytes(bytes)
	s.ephemeralPrivate = ephemeralPrivate
	return s.ephemeralPrivate
}

// makeLittleK initializes multiplier based on group parameters
// k = H(N, g)
// BUG(jpg): Creation of multiplier, little k, does _not_ conform to RFC 5054 padding
func (s *SRP) makeLittleK() (*big.Int, error) {
	if s.group == nil {
		return nil, fmt.Errorf("group not set")
	}

	// We will remake k, even if already created, as server needs to
	// remake it after manually setting k
	h := sha256.New()
	_, err := h.Write(s.group.n.Bytes())
	if err != nil {
		return nil, fmt.Errorf("failed to write N to hasher: %v", err)
	}
	_, err = h.Write(s.group.g.Bytes())
	if err != nil {
		return nil, fmt.Errorf("failed to write g to hasher: %v", err)
	}
	k := &big.Int{}
	s.k = k.SetBytes(h.Sum(nil))
	return s.k, nil
}

// makeA calculates A (if necessary) and returns it
func (s *SRP) makeA() (*big.Int, error) {
	if s.group == nil {
		return nil, fmt.Errorf("group not set")
	}
	if s.isServer {
		return nil, fmt.Errorf("only the client can make A")
	}
	if s.ephemeralPrivate.Cmp(bigZero) == 0 {
		s.ephemeralPrivate = s.generateMySecret()
	}

	s.ephemeralPublicA = &big.Int{}
	result := s.ephemeralPublicA.Exp(s.group.g, s.ephemeralPrivate, s.group.n)
	return result, nil
}

// makeB calculates B and returns it
func (s *SRP) makeB() (*big.Int, error) {

	term1 := &big.Int{}
	term2 := &big.Int{}

	// Absolute Prerequisites: Group, isServer, v
	if s.group == nil {
		return nil, fmt.Errorf("group not set")
	}
	if !s.isServer {
		return nil, fmt.Errorf("only the server can make B")
	}
	if s.v.Cmp(bigZero) == 0 {
		return nil, fmt.Errorf("v must be known before B can be calculated")
	}
	// This test is so I'm not lying to gosec wrt to G105
	if s.group.n.Cmp(bigZero) == 0 {
		return nil, fmt.Errorf("something is wrong if modulus is zero")
	}

	// Generatable prerequisites: k, b if needed
	if s.k.Cmp(bigZero) == 0 {
		var err error
		if s.k, err = s.makeLittleK(); err != nil {
			return nil, err
		}
	}
	if s.ephemeralPrivate.Cmp(bigZero) == 0 {
		s.ephemeralPrivate = s.generateMySecret()
	}

	// B = kv + g^b  (term1 is kv, term2 is g^b)
	// We also do some modular reduction on some of our intermediate values
	term2.Exp(s.group.g, s.ephemeralPrivate, s.group.n) // #nosec G105
	term1.Mul(s.k, s.v)
	term1.Mod(term1, s.group.n)
	s.ephemeralPublicB.Add(term1, term2)
	s.ephemeralPublicB.Mod(s.ephemeralPublicB, s.group.n) // #nosec G105

	return s.ephemeralPublicB, nil
}

func (s *SRP) isUValid() bool {
	if s.u == nil || s.badState {
		s.u = nil
		return false
	}
	if s.u.Cmp(bigZero) == 0 {
		return false
	}
	return true
}

// makeVerifier creates to the verifier from x and parameters
func (s *SRP) makeVerifier() (*big.Int, error) {
	if s.group == nil {
		return nil, fmt.Errorf("group not set")
	}
	if s.badState {
		return nil, fmt.Errorf("we have bad data")
	}
	if s.x.Cmp(bigZero) == 0 {
		return nil, fmt.Errorf("x must be known to calculate v")
	}

	result := s.v.Exp(s.group.g, s.x, s.group.n)

	return result, nil
}

// calculateU creates a hash A and B
// BUG(jpg): Calculation of u does not use RFC 5054 compatable padding/hashing
// The scheme we use (see source) is to use SHA256 of the concatenation of A and B
// each represented as a lowercase hexadecimal string.
// Additionally those hex strings have leading "0" removed even if that makes them of odd length
func (s *SRP) calculateU() (*big.Int, error) {
	if !s.IsPublicValid(s.ephemeralPublicA) || !s.IsPublicValid(s.ephemeralPublicB) {
		s.u = nil
		return nil, fmt.Errorf("both A and B must be known to calculate u")
	}

	h := sha256.New()

	//trimmedHexPublicA := serverStyleHexFromBigInt(s.ephemeralPublicA)
	//trimmedHexPublicB := serverStyleHexFromBigInt(s.ephemeralPublicB)

	_, err := h.Write(s.ephemeralPublicA.Bytes())
	if err != nil {
		return nil, fmt.Errorf("failed to write A to hasher: %v", err)
	}
	_, err = h.Write(s.ephemeralPublicB.Bytes())
	if err != nil {
		return nil, fmt.Errorf("failed to write B to hasher: %v", err)
	}

	u := &big.Int{}
	s.u = u.SetBytes(h.Sum(nil))
	if s.u.Cmp(bigZero) == 0 {
		return nil, fmt.Errorf("u == 0, which is a bad thing")
	}
	return s.u, nil
}

// Convert a bigInt to a lowercase hex string with leading "0"s removed.
// We do this explicitly instead of as an artifact of fmt.Sprintf
func serverStyleHexFromBigInt(bn *big.Int) string {

	// Don't worry. The compiler will build things the same even if we didn't create
	// all of the intermediate variables below. And this better communicates all these
	// things we are doing to construct these strings
	b := bn.Bytes()
	h := hex.EncodeToString(b)
	l := strings.ToLower(h)
	res := strings.TrimLeft(l, "0")

	return res
}

/**
 ** Copyright 2017, 2020 AgileBits, Inc.
 ** Licensed under the Apache License, Version 2.0 (the "License").
 **/
