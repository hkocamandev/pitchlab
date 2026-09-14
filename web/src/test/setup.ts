import "@testing-library/jest-dom/vitest";

// Recharts measures its container, and jsdom reports every element as zero by
// zero, so charts render nothing and their tests assert on an empty SVG.
// Giving the container a size is the standard workaround and it is confined
// here rather than repeated in each chart test.
Object.defineProperty(HTMLElement.prototype, "offsetWidth", {
  configurable: true,
  value: 800,
});
Object.defineProperty(HTMLElement.prototype, "offsetHeight", {
  configurable: true,
  value: 400,
});

globalThis.ResizeObserver = class {
  observe() {}
  unobserve() {}
  disconnect() {}
};
