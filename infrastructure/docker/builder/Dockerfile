FROM golang:1.21.1-alpine3.17 as golang
FROM docker:23.0.6-cli-alpine3.17

COPY --from=golang /usr/local/go/ /usr/local/go/
ENV PATH /usr/local/go/bin:$PATH
RUN apk add --no-cache libgit2 libgit2-dev go protoc make pkgconfig build-base git tar npm sqlite sqlite-dev bash
RUN npm install -g pnpm@8.5.1
RUN wget https://github.com/bufbuild/buf/releases/download/v1.23.1/buf-Linux-x86_64 -O /usr/local/bin/buf && chmod +x /usr/local/bin/buf
RUN echo 'b1f3484cfc092bc14e1f886ff2116a98562bf29df0be2130bbaac128eca0b628  /usr/local/bin/buf' | sha256sum -c
