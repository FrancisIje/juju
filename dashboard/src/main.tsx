import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { App } from "./App";
import { applyTheme, loadTheme } from "./lib/theme";
import "./styles/app.css";

// Apply the saved theme before the first render, so there's no flash of
// the wrong colors.
applyTheme(loadTheme());

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <App />
  </StrictMode>,
);
