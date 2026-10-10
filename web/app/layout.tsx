import type { Metadata } from "next";
import "./globals.css";

export const metadata: Metadata = {
  title: "Sutra Mini · Accounting sync",
  description: "Job-work receipts and reliable accounting sync for Shree Radha Studio.",
};

export default function RootLayout({ children }: Readonly<{ children: React.ReactNode }>) {
  return <html lang="en"><body>{children}</body></html>;
}
