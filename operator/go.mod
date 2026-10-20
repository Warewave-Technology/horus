module github.com/warewave/horus/operator

go 1.24

// The operator lives in its own module so the server binary never pulls in
// client-go/controller-runtime. It shares pkg/api, pkg/client and
// pkg/ingressdns with the server through this replace directive.
//
// Run `go mod tidy` once; it will add:
//   sigs.k8s.io/controller-runtime
//   k8s.io/api, k8s.io/apimachinery, k8s.io/client-go

require github.com/warewave/horus v0.0.0-00010101000000-000000000000

replace github.com/warewave/horus => ../
