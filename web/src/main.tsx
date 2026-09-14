import React from "react";
import ReactDOM from "react-dom/client";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { BrowserRouter } from "react-router-dom";

import { App } from "./App";
import "./styles.css";

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      // Analytics is cached server-side for five minutes and the numbers move
      // slowly; refetching on every window focus would spend requests to
      // redraw the same chart.
      staleTime: 30_000,
      retry: (failureCount, error) => {
        // A 404 is an answer, not a failure to retry.
        if (error instanceof Error && "status" in error && (error as { status: number }).status === 404) {
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
        <App />
      </BrowserRouter>
    </QueryClientProvider>
  </React.StrictMode>,
);
