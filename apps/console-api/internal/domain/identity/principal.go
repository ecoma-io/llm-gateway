package identity

// PrincipalKind says which kind of aggregate authenticated. The two kinds
// are mutually exclusive by construction: exactly one identity field is
// meaningful on any Principal, and the zero kind means "nobody".
type PrincipalKind string

const (
	// PrincipalUser: a console identity authenticated.
	PrincipalUser PrincipalKind = "user"
	// PrincipalAPIKey: an API key credential authenticated.
	PrincipalAPIKey PrincipalKind = "api_key"
)

// Principal is who a verified credential turns out to be: the identity and
// the account it acts for. It is the only thing a verification hands back —
// no secret material, no token, no digest. The field that is not the kind's
// identity is the zero value, and readers must switch on Kind rather than
// guess from emptiness.
type Principal struct {
	Kind      PrincipalKind
	AccountID AccountID
	// UserID is meaningful exactly when Kind is PrincipalUser.
	UserID UserID
	// APIKeyID is meaningful exactly when Kind is PrincipalAPIKey.
	APIKeyID APIKeyID
}

// UserPrincipal builds the Principal for an authenticated user.
func UserPrincipal(accountID AccountID, userID UserID) Principal {
	return Principal{Kind: PrincipalUser, AccountID: accountID, UserID: userID}
}

// APIKeyPrincipal builds the Principal for an authenticated API key.
func APIKeyPrincipal(accountID AccountID, keyID APIKeyID) Principal {
	return Principal{Kind: PrincipalAPIKey, AccountID: accountID, APIKeyID: keyID}
}
