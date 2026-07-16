// Drag-and-drop multi-file upload queue for the photo album (NES-124).
//
// Registers Alpine.data("uploadQueue") for the #upload-dropzone element
// (web/components/photos.templ). Each queued file uploads through its own
// XMLHttpRequest to the existing single-file POST /photos endpoint — fetch()
// cannot report upload progress, so XHR's upload.onprogress event drives the
// per-file progress bar. A small concurrency cap (maxConcurrent) keeps a
// large batch from opening dozens of sockets at once; pump() is the single
// place that decides what starts next, so the cap is easy to find and change.
//
// Each queue item moves through one of these states (item.status):
//   skipped   - failed the client-side type/size pre-check; never uploaded.
//   queued    - waiting for a free upload slot.
//   uploading - XHR in flight; item.progress tracks percent complete.
//   done      - server created a new photo (X-Upload-Result: created).
//   duplicate - server matched an existing photo by content hash.
//   error     - the request failed, timed out, or the server rejected it;
//               retry() moves the item back to "queued".
//
// Every item also carries batchSeq, the enqueueFiles() call it was dropped
// in. maybeFinish() always scopes its "is this batch done, and what's the
// summary" computation to one batchSeq — never the whole (never-trimmed,
// ever-growing) queue — so a second, smaller drop doesn't fold an earlier
// drop's already-reported counts into its own summary line.
//
// UPLOAD_TIMEOUT_MS bounds a single file's request so a hung connection can't
// permanently leak a concurrency slot. Kept just above SERVER_REQUEST_TIMEOUT's
// 2-minute default (internal/platform/config's config.go, SERVER_REQUEST_TIMEOUT
// env var) rather than a shorter value: a legitimate 25 MiB upload on slow
// Wi-Fi can take well over 30s, and this timeout must never fire before the
// server's own request budget would — otherwise the client would abandon an
// upload the server was still willing to accept.
const UPLOAD_TIMEOUT_MS = 150000;

document.addEventListener('alpine:init', () => {
  Alpine.data('uploadQueue', () => ({
    dragActive: false,
    queue: [],
    summary: '',
    maxConcurrent: 4,
    active: 0,
    maxBytes: 0,
    acceptedTypes: [],
    uploadUrl: '',
    batchSeq: 0,

    init() {
      this.maxBytes = parseInt(this.$el.dataset.maxBytes, 10) || 0;
      this.acceptedTypes = (this.$el.dataset.accept || '').split(',').filter(Boolean);
      this.uploadUrl = this.$el.dataset.uploadUrl;
    },

    // enqueueFiles accepts a FileList (from a drop event or the hidden file
    // input's change event) and adds one queue item per file, tagged with a
    // new batchSeq, then starts uploading as many as the concurrency cap
    // allows.
    enqueueFiles(fileList) {
      const files = Array.from(fileList);
      if (files.length === 0) {
        return;
      }
      this.batchSeq += 1;
      const batchSeq = this.batchSeq;
      // Clear the previous batch's summary text immediately so it doesn't
      // linger, confusingly, while this new batch is still in flight; the
      // line this batch eventually resolves is still computed from items
      // scoped to its own batchSeq only (see maybeFinish), never mixed with
      // the batch this text belonged to.
      this.summary = '';
      for (const file of files) {
        this.queue.push(this.buildQueueItem(file, batchSeq));
      }
      this.pump();
      // A batch that is entirely precheck-skipped never reaches
      // startUpload/finishUpload — the only other caller of maybeFinish — so
      // it would otherwise never resolve a summary at all. Calling it here
      // too covers that case; for a batch with real uploads in flight this
      // call simply sees them "still working" and returns, leaving the
      // eventual finishUpload() calls to resolve the summary once they settle.
      this.maybeFinish(batchSeq);
    },

    buildQueueItem(file, batchSeq) {
      const reason = this.precheckReason(file);
      return {
        id: `${Date.now()}-${Math.random().toString(36).slice(2)}`,
        batchSeq,
        file,
        name: file.name,
        progress: 0,
        status: reason ? 'skipped' : 'queued',
        statusLabel: reason || 'queued',
      };
    },

    // precheckReason rejects a file before it is ever enqueued for upload,
    // mirroring the server's accept-list (domain.acceptedContentTypes) and
    // size cap (config.Media.MaxUploadBytes) so an obviously-bad file fails
    // instantly instead of after a wasted round trip. Returns "" when the
    // file passes both checks.
    precheckReason(file) {
      if (this.acceptedTypes.length > 0 && !this.acceptedTypes.includes(file.type)) {
        return 'unsupported type';
      }
      if (this.maxBytes > 0 && file.size > this.maxBytes) {
        return 'too large';
      }
      return '';
    },

    // pump starts uploads for queued items until either the concurrency cap
    // is reached or there is nothing left to start. It is called after every
    // enqueue and after every upload settles, so it is the single place that
    // decides what runs next.
    pump() {
      while (this.active < this.maxConcurrent) {
        const next = this.queue.find((item) => item.status === 'queued');
        if (!next) {
          return;
        }
        this.startUpload(next);
      }
    },

    startUpload(item) {
      item.status = 'uploading';
      item.statusLabel = 'uploading…';
      item.progress = 0;
      this.active += 1;

      const xhr = new XMLHttpRequest();
      xhr.upload.addEventListener('progress', (e) => {
        if (e.lengthComputable) {
          item.progress = Math.round((e.loaded / e.total) * 100);
        }
      });
      xhr.addEventListener('load', () => this.finishUpload(item, xhr));
      xhr.addEventListener('error', () => this.finishUpload(item, null));
      xhr.addEventListener('timeout', () => this.finishUpload(item, null));

      const csrfToken = document.getElementById('upload-csrf-token').value;
      const caption = document.getElementById('upload-caption').value;
      const body = new FormData();
      body.append('csrf_token', csrfToken);
      body.append('caption', caption);
      body.append('photo', item.file, item.file.name);

      xhr.open('POST', this.uploadUrl, true);
      xhr.timeout = UPLOAD_TIMEOUT_MS;
      xhr.send(body);
    },

    // finishUpload settles one item's terminal state (xhr is null on a
    // network-level error or a client-side timeout, both of which XHR
    // reports without a usable status code) and hands the next queued item
    // its freed concurrency slot.
    finishUpload(item, xhr) {
      this.active -= 1;
      if (xhr && xhr.status >= 200 && xhr.status < 300) {
        const result = xhr.getResponseHeader('X-Upload-Result');
        item.status = result === 'duplicate' ? 'duplicate' : 'done';
        item.statusLabel = result === 'duplicate' ? 'duplicate' : 'uploaded';
        item.progress = 100;
      } else {
        item.status = 'error';
        item.statusLabel = 'failed';
      }
      this.pump();
      this.maybeFinish(item.batchSeq);
    },

    // retry re-queues one failed item under its original batchSeq; pump()
    // picks it up on the next free slot, and finishUpload's own maybeFinish()
    // call re-resolves that batch's summary once the retry settles.
    retry(id) {
      const item = this.queue.find((i) => i.id === id);
      if (!item) {
        return;
      }
      item.status = 'queued';
      item.statusLabel = 'queued';
      item.progress = 0;
      this.pump();
    },

    // maybeFinish resolves the summary for one batch (identified by
    // batchSeq, never just "the most recently started one" — an earlier
    // batch can still be settling, or awaiting a retry, after a later one
    // was dropped). Once nothing in that batch is still queued or
    // uploading, it renders the batch's own summary line and — if at least
    // one file in it actually landed (created or duplicate) — triggers a
    // single grid refresh, rather than one per file.
    maybeFinish(batchSeq) {
      const items = this.queue.filter((i) => i.batchSeq === batchSeq);
      const stillWorking = items.some((i) => i.status === 'queued' || i.status === 'uploading');
      if (stillWorking) {
        return;
      }
      const created = items.filter((i) => i.status === 'done').length;
      const duplicates = items.filter((i) => i.status === 'duplicate').length;
      const failed = items.filter((i) => i.status === 'error').length;
      const skipped = items.filter((i) => i.status === 'skipped').length;
      let summary = `${created} uploaded, ${duplicates} duplicates skipped, ${failed} failed`;
      if (skipped > 0) {
        summary += `, ${skipped} skipped (unsupported type or too large)`;
      }
      this.summary = summary;
      if (created > 0 || duplicates > 0) {
        this.refreshGrid();
      }
    },

    // refreshGrid asks the #photo-grid fragment (web/components/photos.templ)
    // to re-render itself via its hx-trigger="photos-uploaded" listener.
    refreshGrid() {
      if (window.htmx) {
        window.htmx.trigger('#photo-grid', 'photos-uploaded');
      }
    },
  }));
});
