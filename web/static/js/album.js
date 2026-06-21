// Rotating photo-album viewer (NES-76). An Alpine component drives the rotation
// cadence; GSAP crossfades between slides and applies a slow Ken Burns pan/zoom.
// prefers-reduced-motion drops the Ken Burns to a plain crossfade, the timer
// pauses while the tab is hidden, and the next image is preloaded to avoid a flash.
document.addEventListener('alpine:init', () => {
  Alpine.data('albumViewer', () => ({
    caption: '',
    colorClass: '',
    index: 0,
    slides: [],
    timer: null,
    rotationMs: 8000,
    reduceMotion: false,

    init() {
      this.slides = Array.from(this.$el.querySelectorAll('.album-slide'));
      const seconds = parseInt(this.$el.dataset.rotationSeconds || '8', 10);
      this.rotationMs = (seconds > 0 ? seconds : 8) * 1000;
      this.reduceMotion = window.matchMedia('(prefers-reduced-motion: reduce)').matches;
      if (this.slides.length === 0) return;

      this.show(0, true);
      if (this.slides.length > 1) {
        this.timer = setInterval(() => this.next(), this.rotationMs);
      }
      document.addEventListener('visibilitychange', () => this.onVisibility());
    },

    show(i, immediate) {
      const slide = this.slides[i];
      const img = slide.querySelector('img');
      const fade = immediate ? 0 : 1.2;

      // Crossfade: bring this slide in (autoAlpha handles opacity + visibility),
      // fade the others out.
      gsap.to(slide, { autoAlpha: 1, duration: fade, ease: 'power1.inOut' });
      this.slides.forEach((s, j) => {
        if (j !== i) gsap.to(s, { autoAlpha: 0, duration: fade, ease: 'power1.inOut' });
      });

      // Ken Burns: a slow zoom + drift over the slide's dwell time (skipped when
      // the user prefers reduced motion).
      if (img && !this.reduceMotion) {
        gsap.fromTo(
          img,
          { scale: 1, xPercent: 0, yPercent: 0 },
          { scale: 1.08, xPercent: -2, yPercent: -2, duration: this.rotationMs / 1000 + 1.2, ease: 'none' },
        );
      }

      this.caption = slide.dataset.caption || '';
      const color = slide.dataset.color || '';
      this.colorClass = color ? `bg-member-${color}-solid` : '';

      // Preload the next image so the upcoming crossfade has nothing to fetch.
      const next = this.slides[(i + 1) % this.slides.length].querySelector('img');
      if (next) {
        const pre = new Image();
        pre.src = next.src;
      }
    },

    next() {
      this.index = (this.index + 1) % this.slides.length;
      this.show(this.index, false);
    },

    onVisibility() {
      if (document.hidden) {
        if (this.timer) {
          clearInterval(this.timer);
          this.timer = null;
        }
      } else if (!this.timer && this.slides.length > 1) {
        this.timer = setInterval(() => this.next(), this.rotationMs);
      }
    },
  }));
});
