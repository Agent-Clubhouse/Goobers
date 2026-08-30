import type { Theme } from "../theme";
import { Icon } from "../ui/Icon";

export function GettingStartedShell({
  children,
  theme,
  toggleTheme,
}: {
  children: React.ReactNode;
  theme: Theme;
  toggleTheme: () => void;
}) {
  return (
    <div className="getting-started-frame">
      <a className="skip-link" href="#getting-started-content">
        Skip to setup
      </a>
      <header className="getting-started-header">
        <div className="getting-started-brand">
          <img alt="" src="/goober-mascot.png" />
          <strong>Goobers</strong>
        </div>
        <button
          aria-label={`Use ${theme === "light" ? "dark" : "light"} theme`}
          className="theme-button"
          onClick={toggleTheme}
          type="button"
        >
          <Icon name={theme === "light" ? "moon" : "sun"} size={17} />
        </button>
      </header>
      <main className="getting-started-content" id="getting-started-content" tabIndex={-1}>
        {children}
      </main>
    </div>
  );
}
