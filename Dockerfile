# Build the NPM UI first
FROM node:22-alpine3.21 AS ui-builder

COPY ui/ /usr/src/app/ui

WORKDIR /usr/src/app/ui

RUN npm install
RUN npm run build

FROM golang:1.24-alpine3.21 AS builder

COPY . /usr/src/app

COPY --from=ui-builder /usr/src/app/ui/dist /usr/src/app/ui/dist
WORKDIR /usr/src/app
RUN go get
RUN CGO_ENABLED=0 go build -o prox-builder .

ENTRYPOINT ["./prox-builder"]