package resources

// AllowLoopbackReports lets a test harness report fake renderers served by
// httptest on 127.0.0.1, which the production SSRF guard refuses.
func AllowLoopbackReports(a *API) { a.allowLoopbackReports = true }
