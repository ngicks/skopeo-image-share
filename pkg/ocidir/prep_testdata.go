package ocidir

//go:generate podman image pull gcr.io/distroless/base-debian12@sha256:9dce90e688a57e59ce473ff7bc4c80bc8fe52d2303b4d99b44f297310bbd2210
//go:generate mkdir ./testdata/share ./testdata/gcr.io/distroless/base-debian12/_digests -p
//go:generate skopeo copy --dest-compress-format zstd --dest-compress-level 10 --dest-shared-blob-dir ./testdata/share containers-storage:gcr.io/distroless/base-debian12@sha256:9dce90e688a57e59ce473ff7bc4c80bc8fe52d2303b4d99b44f297310bbd2210 oci:testdata/gcr.io/distroless/base-debian12/_digests/9dce90e688a57e59ce473ff7bc4c80bc8fe52d2303b4d99b44f297310bbd2210:gcr.io/distroless/base-debian12@sha256:9dce90e688a57e59ce473ff7bc4c80bc8fe52d2303b4d99b44f297310bbd2210
