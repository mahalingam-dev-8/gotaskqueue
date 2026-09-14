# Multi-stage build. One Dockerfile builds both binaries; pick with --build-arg.
#
#   docker build --build-arg SERVICE=api    -t gotaskqueue-api .
#   docker build --build-arg SERVICE=worker -t gotaskqueue-worker .

# ---------- build ----------
FROM golang:1.25-alpine AS build

WORKDIR /src

# Dependencies are copied and downloaded first so this layer is cached until
# go.mod/go.sum actually change.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG SERVICE=api
ARG VERSION=dev

# CGO_ENABLED=0 produces a static binary with no libc dependency, which is what
# makes the distroless/scratch final stage possible.
# -trimpath strips local paths; -s -w drop the symbol table and DWARF info.
RUN CGO_ENABLED=0 GOOS=linux go build \
	-trimpath \
	-ldflags="-s -w -X main.version=${VERSION}" \
	-o /out/app ./cmd/${SERVICE}

# ---------- runtime ----------
# distroless/static has no shell and no package manager: a much smaller attack
# surface than alpine, and the image is ~2MB plus your binary.
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /
COPY --from=build /out/app /app

USER nonroot:nonroot
EXPOSE 8080

ENTRYPOINT ["/app"]
