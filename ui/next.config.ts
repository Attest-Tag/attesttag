import type { NextConfig } from "next";

// The console is a static export served by the Go binary at /admin/ (see
// ui/embed.go). Every page is client-rendered against the relative /api/*
// routes on the same origin, so nothing here needs a Node server.
const nextConfig: NextConfig = {
  output: "export",
  trailingSlash: true,
  basePath: "/admin",
  images: { unoptimized: true },
  poweredByHeader: false,
};

export default nextConfig;
