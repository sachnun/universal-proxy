# Universal Proxy

A simple proxy server that allows you to access any website through a local proxy with URL rewriting.

## Installation

```bash
go build
```

## Usage

### Local Binary

```bash
./universal-proxy
```

### Docker

```bash
docker run -p 8080:8080 ghcr.io/sachnun/universal-proxy:latest
```

## How to Use

Once the proxy server is running on port 8080, you can access any website through the proxy by using the following URL format:

```
http://localhost:8080/{domain}/{path}
```

### Examples

- Access Google: `http://localhost:8080/google.com`
- Access GitHub: `http://localhost:8080/github.com/sachnun/universal-proxy`
- Access Wikipedia: `http://localhost:8080/wikipedia.org/wiki/Proxy_server`

### Features

- **URL Rewriting**: Automatically rewrites absolute URLs, relative URLs, and CSS URLs to work through the proxy
- **HTTPS Support**: Proxies requests to HTTPS websites
- **Path Preservation**: Maintains the original path structure
- **Content Modification**: Modifies HTML content to ensure all links work through the proxy

### How It Works

1. The proxy receives a request like `http://localhost:8080/example.com/path/to/page`
2. It extracts the domain (`example.com`) and the remaining path (`/path/to/page`)
3. It forwards the request to `https://example.com/path/to/page`
4. When the response comes back, it rewrites all URLs in the HTML to point back through the proxy
5. The modified response is sent back to your browser

This allows you to browse any website through the proxy while maintaining proper functionality of all links and resources.

