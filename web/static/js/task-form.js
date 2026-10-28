// New-task form state (NES-211): an "as needed" task has no rotation, so it can
// only be claimable. Registers Alpine.data('taskForm') for the form wrapper in
// web/components/newtask.templ.
//
// Initial state arrives as JSON in the element's data-state attribute (built
// by alpineTaskState in newtask_helpers.go). It is parsed, never evaluated, so
// a hostile sticky value cannot become script. The rule previously lived in an
// x-effect statement block, which the Alpine CSP build cannot evaluate.
document.addEventListener('alpine:init', () => {
  Alpine.data('taskForm', () => ({
    freq: '',
    policy: '',

    init() {
      Object.assign(this, JSON.parse(this.$el.dataset.state));
      this.forceClaimableWhenAsNeeded();
      this.$watch('freq', () => this.forceClaimableWhenAsNeeded());
    },

    forceClaimableWhenAsNeeded() {
      if (this.freq === 'as_needed') {
        this.policy = 'claimable';
      }
    },
  }));
});
