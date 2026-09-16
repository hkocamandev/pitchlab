import React from "react";
import ReactDOM from "react-dom/client";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { BrowserRouter } from "react-router-dom";

import { App } from "./App";
import { AuthProvider } from "./auth/AuthContext";
import "./styles.css";

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      // Analytics is cached server-side for five minutes and the numbers move
      // slowly; refetching on every window focus would spend requests to
      // redraw the same chart.
      staleTime: 30_000,
      retry: (failureCount, error) => {
        const status =
          error instanceof Error && "status" in error
            ? (error as { status: number }).status
            : 0;
        // A 404 is an answer, not a failure to retry. A 401 or a 403 is an
        // answer too, and retrying it is worse than useless: three attempts
        // per query against a throttled endpoint is how a lost session turns
        // into a lockout.
        if (status === 404 || status === 401 || status === 403) {
          return false;
        }
        return failureCount < 2;
      },
    },
  },
});

ReactDOM.createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <QueryClientProvider client={queryClient}>
      <BrowserRouter>
        <AuthProvider>
          <App />
        </AuthProvider>
      </BrowserRouter>
    </QueryClientProvider>
  </React.StrictMode>,
);
