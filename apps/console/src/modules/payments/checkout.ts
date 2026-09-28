// The one place this console sends the browser off its own origin.
//
// `checkout_url` is the provider's hosted page, and the contract is explicit
// that it is "returned verbatim and never parsed, rewritten or re-derived by
// anything on this side". So this function does exactly one thing with it: it
// assigns it to the browser. It does not fetch it, does not preflight it, does
// not read a session id out of it, and does not decide whether it looks
// reasonable — a client that did any of those would break the day the
// provider's URL shape changed, and would be reading a checkout it has no
// business in.
//
// It is a module of its own rather than a line in the page because navigation
// is the one side effect a test cannot observe in jsdom, and a screen that
// navigated inline would be a screen whose single most important behaviour —
// sending the customer to the PROVIDER rather than collecting anything here —
// could only be tested by accident.
export function sendToCheckout(url: string): void {
  window.location.assign(url);
}
