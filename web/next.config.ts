import type { NextConfig } from "next";

const config: NextConfig = {
  output: "standalone",
  poweredByHeader: false,
  async rewrites() {
    const apiURL = (process.env.API_URL || "http://localhost:8080").replace(/\/$/, "");
    return [{ source: "/api/:path*", destination: `${apiURL}/:path*` }];
  },
};

export default config;
