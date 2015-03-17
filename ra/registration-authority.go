// Copyright 2014 ISRG.  All rights reserved
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package ra

import (
	"crypto/x509"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/letsencrypt/boulder/core"
	"github.com/letsencrypt/boulder/jose"
)

// All of the fields in RegistrationAuthorityImpl need to be
// populated, or there is a risk of panic.
type RegistrationAuthorityImpl struct {
	CA core.CertificateAuthority
	VA core.ValidationAuthority
	SA core.StorageAuthority

	AuthzBase string
}

func NewRegistrationAuthorityImpl() RegistrationAuthorityImpl {
	return RegistrationAuthorityImpl{}
}

var dnsLabelRegexp, _ = regexp.Compile("^[a-zA-Z0-9-]*$")
var ipAddressRegexp, _ = regexp.Compile("^[0-9.]*$")

func forbiddenIdentifier(id string) bool {
	// A DNS label is a part separated by dots, e.g. www.foo.net has labels
	// "www", "foo", and "net".
	const maxLabels = 10
	labels := strings.SplitN(id, ".", maxLabels+1)
	if len(labels) < 2 || len(labels) > maxLabels {
		return true
	}

	for _, label := range labels {
		// DNS defines max label length as 63 characters. Some implementations allow
		// more, but we will be conservative.
		if len(label) < 1 || len(label) > 63 {
			return true
		}
		// Only alphanumerics and dash are allowed in identifiers.
		// TODO: Before identifiers reach this function, do lowercasing.
		if !dnsLabelRegexp.MatchString(label) {
			return true
		}

		// A label cannot begin with a hyphen (-)
		if label[0] == '-' {
			return true
		}

		// Punycode labels are not yet allowed. May allow in future after looking at
		// homoglyph mitigations.
		if len(label) >= 4 && label[0:4] == "xn--" {
			return true
		}
	}

	// Forbid identifiers that are entirely numeric like an IP address.
	if ipAddressRegexp.MatchString(id) {
		return true
	}

	// Also forbid an all-numeric final label.
	if ipAddressRegexp.MatchString(labels[len(labels)-1]) {
		return true
	}

	return false
}

var allButLastPathSegment = regexp.MustCompile("^.*/")

func lastPathSegment(url core.AcmeURL) string {
	return allButLastPathSegment.ReplaceAllString(url.Path, "")
}

func (ra *RegistrationAuthorityImpl) NewRegistration(init core.Registration, key jose.JsonWebKey) (reg core.Registration, err error) {
	regID, err := ra.SA.NewRegistration()
	if err != nil {
		return
	}

	reg = core.Registration{
		ID:            regID,
		Key:           key,
		RecoveryToken: core.NewToken(),
	}
	reg.MergeUpdate(init)

	// Store the authorization object, then return it
	err = ra.SA.UpdateRegistration(reg)
	return
}

func (ra *RegistrationAuthorityImpl) NewAuthorization(request core.Authorization, key jose.JsonWebKey) (authz core.Authorization, err error) {
	identifier := request.Identifier

	// Check that the identifier is present and appropriate
	if len(identifier.Value) == 0 {
		err = core.MalformedRequestError("No identifier in authorization request")
		return
	} else if identifier.Type != core.IdentifierDNS {
		err = core.NotSupportedError("Only domain validation is supported")
		return
	} else if forbiddenIdentifier(identifier.Value) {
		err = core.UnauthorizedError("We will not authorize use of this identifier")
		return
	}

	// Create validations
	// TODO: Assign URLs
	simpleHttps := core.SimpleHTTPSChallenge()
	dvsni := core.DvsniChallenge()
	authID, err := ra.SA.NewPendingAuthorization()
	if err != nil {
		return
	}
	// Ignoring these errors because we construct the URLs to be correct
	simpleHTTPSURI, _ := url.Parse(ra.AuthzBase + authID + "?" + core.RandomString(4))
	dvsniURI, _ := url.Parse(ra.AuthzBase + authID + "?" + core.RandomString(4))
	simpleHttps.URI = core.AcmeURL(*simpleHTTPSURI)
	dvsni.URI = core.AcmeURL(*dvsniURI)

	// Create a new authorization object
	authz = core.Authorization{
		ID:         authID,
		Identifier: identifier,
		Key:        key,
		Status:     core.StatusPending,
		Challenges: []core.Challenge{
			simpleHttps,
			dvsni,
		},
	}

	// Store the authorization object, then return it
	err = ra.SA.UpdatePendingAuthorization(authz)
	return
}

func (ra *RegistrationAuthorityImpl) NewCertificate(req core.CertificateRequest, jwk jose.JsonWebKey) (cert core.Certificate, err error) {
	// Verify the CSR
	// TODO: Verify that other aspects of the CSR are appropriate
	csr := req.CSR
	err = core.VerifyCSR(csr)
	if err != nil {
		err = core.UnauthorizedError("Invalid signature on CSR")
		return
	}

	// Gather authorized domains from the referenced authorizations
	authorizedDomains := map[string]bool{}
	now := time.Now()
	for _, url := range req.Authorizations {
		id := lastPathSegment(url)
		authz, err := ra.SA.GetAuthorization(id)
		if err != nil || // Couldn't find authorization
			!jwk.Equals(authz.Key) || // Not for the right account key
			authz.Status != core.StatusValid || // Not finalized or not successful
			authz.Expires.Before(now) || // Expired
			authz.Identifier.Type != core.IdentifierDNS {
			// XXX: It may be good to fail here instead of ignoring invalid authorizations.
			//      However, it seems like this treatment is more in the spirit of Postel's
			//      law, and it hides information from attackers.
			continue
		}

		authorizedDomains[authz.Identifier.Value] = true
	}

	// Validate that authorization key is authorized for all domains
	names := csr.DNSNames
	if len(csr.Subject.CommonName) > 0 {
		names = append(names, csr.Subject.CommonName)
	}
	for _, name := range names {
		if !authorizedDomains[name] {
			err = core.UnauthorizedError(fmt.Sprintf("Key not authorized for name %s", name))
			return
		}
	}

	// Create the certificate
	cert, err = ra.CA.IssueCertificate(*csr)
	return
}

func (ra *RegistrationAuthorityImpl) UpdateRegistration(base core.Registration, update core.Registration) (reg core.Registration, err error) {
	base.MergeUpdate(update)
	reg = base
	err = ra.SA.UpdateRegistration(base)
	return
}

func (ra *RegistrationAuthorityImpl) UpdateAuthorization(base core.Authorization, challengeIndex int, response core.Challenge) (authz core.Authorization, err error) {
	// Copy information over that the client is allowed to supply
	authz = base
	if challengeIndex >= len(authz.Challenges) {
		err = core.MalformedRequestError("Invalid challenge index")
		return
	}
	authz.Challenges[challengeIndex] = authz.Challenges[challengeIndex].MergeResponse(response)

	// Store the updated version
	ra.SA.UpdatePendingAuthorization(authz)

	// Dispatch to the VA for service
	ra.VA.UpdateValidations(authz)

	return
}

func (ra *RegistrationAuthorityImpl) RevokeCertificate(cert x509.Certificate) error {
	// TODO: ra.CA.RevokeCertificate()
	return nil
}

func (ra *RegistrationAuthorityImpl) OnValidationUpdate(authz core.Authorization) {
	// Check to see whether the updated validations are sufficient
	// Current policy is to accept if any validation succeeded
	for _, val := range authz.Challenges {
		if val.Status == core.StatusValid {
			authz.Status = core.StatusValid
			break
		}
	}

	// If no validation succeeded, then the authorization is invalid
	// NOTE: This only works because we only ever do one validation
	if authz.Status != core.StatusValid {
		authz.Status = core.StatusInvalid
	} else {
		// TODO: Enable configuration of expiry time
		authz.Expires = time.Now().Add(365 * 24 * time.Hour)
	}

	// Finalize the authorization
	ra.SA.FinalizeAuthorization(authz)
}