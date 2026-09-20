import { useEffect, useRef } from 'react';
import { useDocumentVisible, useMediaQuery } from '../lib/hooks';

/**
 * Ambient visual layer: a star field whose density and drift track live token
 * throughput, request pulses that travel across the field as traffic arrives,
 * and an aurora whose warmth follows cost velocity.
 *
 * It is decorative and strictly optional. The layer unmounts entirely when the
 * viewer prefers reduced motion, on narrow viewports, while the tab is hidden,
 * or when the frame budget is missed — layout is identical either way.
 */

export interface AmbientSignal {
  /** Combined tokens per minute across the gateway, used for star density and drift. */
  tokensPerMinute: number;
  /** Input tokens per minute; rendered by the shell readout, unused by the canvas. */
  tokensInPerMinute: number;
  /** Output tokens per minute; rendered by the shell readout, unused by the canvas. */
  tokensOutPerMinute: number;
  /** Requests per minute, used for pulse frequency. */
  requestsPerMinute: number;
  /** Spend velocity in USD per minute, used for aurora warmth. */
  costPerMinute: number;
  /** Share of recent requests that failed, tinting shooting stars toward the error hue. */
  errorRate: number;
  /**
   * Recent completed requests, newest first. Each becomes exactly one shooting
   * star, sized by its own output-token count — so what you see crossing the
   * screen is real work the gateway did, not decoration on a timer.
   */
  recentJobs: AmbientJob[];
}

/** One completed request, reduced to what the animation needs. */
interface AmbientJob {
  /** Epoch milliseconds; used to fire stars in arrival order, once each. */
  at: number;
  tokensOut: number;
  error: boolean;
}

interface Star {
  x: number;
  y: number;
  z: number;
  radius: number;
  /**
   * 0..1 fade level. New stars fade in and retiring ones fade out, so a
   * density change is a drift rather than a jump cut.
   */
  fade: number;
  /** Retiring stars fade out and are removed once invisible. */
  retiring: boolean;
}

/**
 * A shooting star is one real request crossing the sky. Length and brightness
 * scale with the job's output tokens, so a big generation is visibly bigger
 * than a one-line reply.
 */
interface ShootingStar {
  progress: number;
  lane: number;
  /** Vertical drift so trails are not all perfectly horizontal. */
  slope: number;
  /** Trail length in px, derived from tokens out. */
  length: number;
  /** Head radius in px, derived from tokens out. */
  size: number;
  speed: number;
  tone: 'normal' | 'error';
}

const MIN_STARS = 40;
const MAX_STARS = 900;
const MAX_SHOOTING_STARS = 24;
const DEGRADE_FPS = 45;

/**
 * Tokens-out → trail length, at roughly the requested 1px per 1000 tokens but
 * on a curve: a linear mapping makes a 200k-token job a screen-wide streak
 * while every ordinary reply is invisible. The square root keeps small jobs
 * legible and stops big ones dominating, and the result is clamped so nothing
 * ever spans the viewport.
 */
const MIN_TRAIL = 18;
const MAX_TRAIL = 190;
export function trailLengthForTokens(tokensOut: number): number {
  const linear = Math.max(0, tokensOut) / 1000;
  const eased = Math.sqrt(linear) * 26;
  return clamp(MIN_TRAIL + eased, MIN_TRAIL, MAX_TRAIL);
}

/** Head size follows the same curve on a tighter range. */
export function headSizeForTokens(tokensOut: number): number {
  const linear = Math.max(0, tokensOut) / 1000;
  return clamp(0.9 + Math.sqrt(linear) * 0.5, 0.9, 3.2);
}

function clamp(value: number, min: number, max: number): number {
  return Math.max(min, Math.min(max, value));
}

interface Rgb {
  r: number;
  g: number;
  b: number;
}

/**
 * Canvas fillStyle cannot consume `var(--janus-*)` directly, so token values
 * are resolved to RGB once per theme via getComputedStyle. The fallbacks only
 * matter in non-DOM test environments where computed styles are empty.
 */
function parseColor(value: string, fallback: Rgb): Rgb {
  const v = value.trim();
  const hex = /^#([0-9a-f]{3}|[0-9a-f]{6})$/i.exec(v);
  if (hex && hex[1]) {
    let digits = hex[1];
    if (digits.length === 3) {
      digits = digits
        .split('')
        .map((c) => c + c)
        .join('');
    }
    return {
      r: parseInt(digits.slice(0, 2), 16),
      g: parseInt(digits.slice(2, 4), 16),
      b: parseInt(digits.slice(4, 6), 16),
    };
  }
  const rgb = /^rgba?\(\s*(\d+)[,\s]+(\d+)[,\s]+(\d+)/i.exec(v);
  if (rgb) return { r: Number(rgb[1]), g: Number(rgb[2]), b: Number(rgb[3]) };
  return fallback;
}

function rgba(color: Rgb, alpha: number): string {
  return `rgba(${color.r}, ${color.g}, ${color.b}, ${alpha})`;
}

/** Resolves every colour the canvas paints from the --janus-* token ramp. */
function resolvePalette() {
  const styles = getComputedStyle(document.documentElement);
  const token = (name: string, fallback: Rgb) => parseColor(styles.getPropertyValue(name), fallback);
  return {
    starBright: token('--janus-color-text-primary', { r: 236, g: 238, b: 251 }),
    starDim: token('--janus-color-text-muted', { r: 95, g: 103, b: 134 }),
    auroraCool: token('--janus-color-primary-bg', { r: 124, g: 134, b: 255 }),
    auroraWarm: token('--janus-color-accent-bg', { r: 251, g: 191, b: 36 }),
    pulseNormal: token('--janus-color-primary-bg', { r: 124, g: 134, b: 255 }),
    pulseError: token('--janus-color-danger-solid', { r: 248, g: 113, b: 113 }),
  };
}

export function AmbientCanvas({ signal, reducedMotion }: { signal: AmbientSignal; reducedMotion: boolean }) {
  const canvasRef = useRef<HTMLCanvasElement>(null);
  const signalRef = useRef(signal);
  const isNarrow = useMediaQuery('(max-width: 767px)');
  const visible = useDocumentVisible();

  signalRef.current = signal;

  const enabled = !reducedMotion && !isNarrow && visible;

  useEffect(() => {
    if (!enabled) return undefined;
    const canvas = canvasRef.current;
    if (!canvas) return undefined;
    const context = canvas.getContext('2d', { alpha: true });
    if (!context) return undefined; // no 2D context: skip the layer entirely

    let width = 0;
    let height = 0;
    let stars: Star[] = [];
    let shootingStars: ShootingStar[] = [];
    // Job timestamps already turned into a star, so each request fires once.
    const firedJobs = new Set<number>();
    // False until the first spawn pass has absorbed the initial backlog.
    let primed = false;
    let frame = 0;
    let quality = 1; // scaled down when the frame budget is missed
    let lastFrameTime = performance.now();
    let slowFrames = 0;
    let stopped = false;
    let palette = resolvePalette();

    // The token values change with the theme; re-resolve when it flips.
    const themeObserver = new MutationObserver(() => {
      palette = resolvePalette();
    });
    themeObserver.observe(document.documentElement, { attributes: true, attributeFilter: ['data-theme'] });

    const dpr = Math.min(window.devicePixelRatio || 1, 2);

    const resize = () => {
      width = canvas.clientWidth;
      height = canvas.clientHeight;
      canvas.width = Math.floor(width * dpr);
      canvas.height = Math.floor(height * dpr);
      context.setTransform(dpr, 0, 0, dpr, 0, 0);
      seedStars();
    };

    const targetStarCount = () => {
      const throughput = signalRef.current.tokensPerMinute;
      // Log scale: a quiet gateway still shows a sky, a busy one fills it.
      const scaled = Math.log10(Math.max(1, throughput)) / 6;
      return Math.round(clamp(MIN_STARS + scaled * (MAX_STARS - MIN_STARS), MIN_STARS, MAX_STARS) * quality);
    };

    // A brand-new star, either scattered across the field (initial seed) or
    // entering from the left edge to drift in with everything else.
    const makeStar = (scattered: boolean): Star => ({
      x: scattered ? Math.random() * width : -Math.random() * 40,
      y: Math.random() * height,
      z: Math.random() * 0.9 + 0.1,
      radius: Math.random() * 1.5 + 1,
      // A scattered star is already "there"; an arriving one fades up.
      fade: scattered ? 1 : 0,
      retiring: false,
    });

    // Full reseed. Only on first paint and on resize, where the canvas
    // dimensions genuinely changed and every position is invalid anyway.
    const seedStars = () => {
      stars = Array.from({ length: targetStarCount() }, () => makeStar(true));
    };

    /**
     * Move the field toward the target density WITHOUT a visible reset.
     *
     * Previously this called seedStars(), which replaced every star at once —
     * the whole sky blinked just to change how many stars were in it. Now new
     * stars enter from the left and fade up, surplus stars are marked retiring
     * and fade out as they drift, and the change is applied a few stars at a
     * time so even a large swing reads as the field thickening or thinning.
     */
    const adjustDensity = () => {
      const target = targetStarCount();
      const live = stars.filter((s) => !s.retiring);
      const drift = live.length - target;
      // Cap per-tick churn so a 10x traffic spike ramps in over a few
      // seconds instead of dumping hundreds of stars in one frame.
      const step = Math.max(1, Math.round(Math.abs(drift) * 0.34));
      if (drift < 0) {
        for (let i = 0; i < Math.min(step, -drift); i += 1) stars.push(makeStar(false));
      } else if (drift > 0) {
        // Retire the dimmest (most distant) first: the sky thins from the
        // back, which is far less noticeable than losing foreground stars.
        const ordered = [...live].sort((a, b) => a.z - b.z);
        for (let i = 0; i < Math.min(step, drift); i += 1) {
          const star = ordered[i];
          if (star) star.retiring = true;
        }
      }
    };

    const drawAurora = () => {
      const warmth = clamp(signalRef.current.costPerMinute / 2, 0, 1);
      const intensity = 0.1 + warmth * 0.2;

      const cool = context.createRadialGradient(width * 0.2, 0, 0, width * 0.2, 0, Math.max(width, height) * 0.8);
      cool.addColorStop(0, rgba(palette.auroraCool, intensity));
      cool.addColorStop(1, rgba(palette.auroraCool, 0));
      context.fillStyle = cool;
      context.fillRect(0, 0, width, height);

      const warm = context.createRadialGradient(
        width * 0.85,
        height * 0.1,
        0,
        width * 0.85,
        height * 0.1,
        Math.max(width, height) * 0.7,
      );
      warm.addColorStop(0, rgba(palette.auroraWarm, intensity * warmth));
      warm.addColorStop(1, rgba(palette.auroraWarm, 0));
      context.fillStyle = warm;
      context.fillRect(0, 0, width, height);
    };

    const drawStars = (delta: number) => {
      const throughput = signalRef.current.tokensPerMinute;
      const speed = clamp(0.02 + Math.log10(Math.max(1, throughput)) / 60, 0.02, 0.35);
      // ~1.2s to fade fully in or out: slow enough to read as a drift, fast
      // enough that the field tracks a traffic change without lagging it.
      const fadeStep = delta / 1200;
      for (const star of stars) {
        star.x += speed * star.z * delta * 0.06;
        if (star.x > width) {
          // A retiring star that reaches the edge has finished its journey:
          // let it leave rather than wrapping it around again.
          if (star.retiring) {
            star.fade = 0;
            continue;
          }
          star.x = 0;
        }
        star.fade = clamp(star.fade + (star.retiring ? -fadeStep : fadeStep), 0, 1);
        if (star.fade <= 0) continue;
        context.globalAlpha = (0.25 + star.z * 0.3) * star.fade;
        context.fillStyle = rgba(star.z > 0.75 ? palette.starBright : palette.starDim, 1);
        context.beginPath();
        context.arc(star.x, star.y, star.radius * star.z, 0, Math.PI * 2);
        context.fill();
      }
      context.globalAlpha = 1;
      // Reap fully faded stars once per frame.
      if (stars.some((s) => s.retiring && s.fade <= 0)) {
        stars = stars.filter((s) => !(s.retiring && s.fade <= 0));
      }
    };

    // Each job fires exactly once. Ids are the job timestamps, which the API
    // returns newest-first; anything already seen is skipped so a poll that
    // re-reports the same request does not re-fire its star.
    const spawnShootingStars = () => {
      if (quality < 0.5) return; // trails are dropped first under load
      const jobs = signalRef.current.recentJobs;
      if (!jobs || jobs.length === 0) return;
      for (const job of jobs) {
        if (firedJobs.has(job.at)) continue;
        firedJobs.add(job.at);
        // On the very first frame after load the whole backlog is marked as
        // seen without drawing, otherwise arriving at the page fires two
        // hours of history at once.
        if (!primed) continue;
        if (shootingStars.length >= MAX_SHOOTING_STARS) continue;
        shootingStars.push({
          progress: 0,
          lane: 0.08 + Math.random() * 0.84,
          slope: (Math.random() - 0.5) * 0.22,
          length: trailLengthForTokens(job.tokensOut),
          size: headSizeForTokens(job.tokensOut),
          // Bigger jobs travel slightly slower, so they read as more massive.
          speed: 1 / (1100 + job.tokensOut / 40),
          tone: job.error ? 'error' : 'normal',
        });
      }
      primed = true;
      // The id set only needs to cover the API window; bound it so a long
      // session cannot grow it without limit.
      if (firedJobs.size > 400) {
        const keep = jobs.map((j) => j.at);
        firedJobs.clear();
        for (const id of keep) firedJobs.add(id);
      }
    };

    const drawShootingStars = (delta: number) => {
      shootingStars = shootingStars.filter((s) => s.progress < 1);
      for (const star of shootingStars) {
        star.progress += delta * star.speed;
        const p = clamp(star.progress, 0, 1);
        const x = p * (width + star.length) - star.length;
        const y = star.lane * height + p * star.slope * height;
        const color = star.tone === 'error' ? palette.pulseError : palette.pulseNormal;
        // Fade in and out so a trail never pops at either edge.
        const alpha = Math.sin(p * Math.PI) * 0.9;

        // The trail: a short gradient streak behind the head, not a giant
        // soft blob. Length and head size both come from the job's tokens.
        const tailX = x - star.length;
        const tailY = y - star.slope * star.length * 0.25;
        const trail = context.createLinearGradient(tailX, tailY, x, y);
        trail.addColorStop(0, rgba(color, 0));
        trail.addColorStop(1, rgba(color, alpha * 0.55));
        context.strokeStyle = trail;
        context.lineWidth = star.size;
        context.lineCap = 'round';
        context.beginPath();
        context.moveTo(tailX, tailY);
        context.lineTo(x, y);
        context.stroke();

        // The head: a small bright point, brighter than the trail.
        context.fillStyle = rgba(palette.starBright, alpha);
        context.beginPath();
        context.arc(x, y, star.size * 0.75, 0, Math.PI * 2);
        context.fill();
      }
    };

    const render = (now: number) => {
      if (stopped) return;
      const delta = Math.min(now - lastFrameTime, 64);
      lastFrameTime = now;

      // Degrade gracefully rather than stuttering: thin the sky, then drop
      // pulses, then unmount the layer entirely.
      const fps = delta > 0 ? 1000 / delta : 60;
      if (fps < DEGRADE_FPS) {
        slowFrames += 1;
        if (slowFrames > 45 && quality > 0.25) {
          quality = Math.max(0.25, quality / 2);
          // Thin the sky by retiring stars gradually rather than reseeding:
          // a frame-rate dip should not also cause a visible blink.
          adjustDensity();
          slowFrames = 0;
        } else if (slowFrames > 180) {
          stopped = true;
          context.clearRect(0, 0, width, height);
          return;
        }
      } else if (slowFrames > 0) {
        slowFrames -= 1;
      }

      context.clearRect(0, 0, width, height);
      drawAurora();
      drawStars(delta);
      spawnShootingStars();
      drawShootingStars(delta);
      frame = window.requestAnimationFrame(render);
    };

    resize();
    frame = window.requestAnimationFrame(render);
    window.addEventListener('resize', resize);

    // Track live traffic by nudging the field toward the target density.
    // This runs often with a small step so the sky thickens and thins
    // continuously; it never reseeds, which is what caused the visible reset.
    const density = window.setInterval(() => {
      const desired = targetStarCount();
      const live = stars.filter((s) => !s.retiring).length;
      // A small dead band stops the field churning on noise.
      if (Math.abs(desired - live) > Math.max(4, desired * 0.05)) {
        adjustDensity();
      }
    }, 700);

    return () => {
      stopped = true;
      themeObserver.disconnect();
      window.cancelAnimationFrame(frame);
      window.removeEventListener('resize', resize);
      window.clearInterval(density);
    };
  }, [enabled]);

  if (!enabled) {
    // The static counterpart: identical layout, no animation.
    return (
      <div
        aria-hidden="true"
        style={{
          position: 'fixed',
          inset: 0,
          zIndex: -1,
          background: 'var(--janus-gradient-aurora)',
          pointerEvents: 'none',
        }}
      />
    );
  }

  return (
    <canvas
      ref={canvasRef}
      aria-hidden="true"
      style={{
        position: 'fixed',
        inset: 0,
        zIndex: -1,
        width: '100%',
        height: '100%',
        pointerEvents: 'none',
      }}
    />
  );
}
