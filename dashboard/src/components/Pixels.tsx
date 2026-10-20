import { useEffect, useRef } from "react";

// A pixel mosaic in the brand's pixel colors: juju's nod to the playful,
// pixel-art side of the reference style. Drawn on a canvas (not hundreds of
// DOM nodes), from a fixed seed so it looks the same on every visit.
export function Pixels({ seed = 7, density = 0.16, cell = 14, className }: { seed?: number; density?: number; cell?: number; className?: string }) {
  const ref = useRef<HTMLCanvasElement>(null);
  useEffect(() => {
    const canvas = ref.current;
    if (!canvas) return;
    const draw = () => {
      const { width, height } = canvas.getBoundingClientRect();
      const ratio = window.devicePixelRatio || 1;
      canvas.width = Math.max(1, Math.round(width * ratio));
      canvas.height = Math.max(1, Math.round(height * ratio));
      const ctx = canvas.getContext("2d");
      if (!ctx) return;
      ctx.scale(ratio, ratio);
      const css = getComputedStyle(canvas);
      // Colors come from the --pixel-* tokens; without them, draw nothing.
      const colors = [1, 2, 3, 4].map((i) => css.getPropertyValue(`--pixel-${i}`).trim()).filter(Boolean);
      if (colors.length === 0) return;
      let s = seed;
      const rand = () => ((s = (s * 16807) % 2147483647) - 1) / 2147483646; // a tiny seeded generator
      const cols = Math.ceil(width / cell);
      const rows = Math.ceil(height / cell);
      for (let y = 0; y < rows; y++) {
        for (let x = 0; x < cols; x++) {
          // Denser toward the top right, fading out: shapes, not static.
          const bias = (x / cols) * (1 - y / rows);
          if (rand() < density * (0.3 + bias * 1.6)) {
            ctx.fillStyle = colors[Math.floor(rand() * colors.length)]!;
            ctx.fillRect(x * cell, y * cell, cell - 2, cell - 2);
          }
        }
      }
    };
    draw();
    const ro = new ResizeObserver(draw);
    ro.observe(canvas);
    return () => ro.disconnect();
  }, [seed, density, cell]);
  return <canvas ref={ref} className={className} aria-hidden="true" />;
}
