# Dockerfile extending the generic Go image with application files for a
# single application.
FROM gcr.io/google_appengine/golang

RUN apt-get update && apt-get install -y git && rm -rf /var/lib/apt/lists/*

COPY . /go/src/app
RUN go-wrapper install -tags appenginevm
