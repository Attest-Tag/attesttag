"use client";

import { createContext, useContext, useEffect } from "react";
import { api, UNAUTHORIZED_EVENT, useApi, type Me } from "@/lib/api";

type AuthState = {
  me: Me | undefined;
  loading: boolean;
  error: string | undefined;
  reload: () => void;
  signOut: () => Promise<void>;
};

const AuthContext = createContext<AuthState | null>(null);

export function useAuth() {
  const ctx = useContext(AuthContext);
  if (!ctx) throw new Error("useAuth must be used inside AuthProvider");
  return ctx;
}

// Loads /api/me once and re-checks whenever any call comes back 401, so an
// expired session drops the console to the login card instead of leaving
// every button failing quietly.
export function AuthProvider({ children }: { children: React.ReactNode }) {
  const { data, loading, error, reload } = useApi<Me>("/api/me");

  useEffect(() => {
    window.addEventListener(UNAUTHORIZED_EVENT, reload);
    return () => window.removeEventListener(UNAUTHORIZED_EVENT, reload);
  }, [reload]);

  const signOut = async () => {
    await api.post("/api/auth/logout");
    window.location.reload();
  };

  return (
    <AuthContext.Provider value={{ me: data, loading, error, reload, signOut }}>
      {children}
    </AuthContext.Provider>
  );
}
