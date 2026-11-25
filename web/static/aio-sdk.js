// AIO SDK for custom plugin UIs (loaded inside the tool iframe).
//
//   <link rel="stylesheet" href="/style.css">
//   <script src="/aio-sdk.js"></script>
//
//   const job = await AIO.submit({
//     files: { files: [file1, file2] },   // field name -> File[]
//     params: { pages: [...] },           // arbitrary JSON, merged into params.json
//     fields: { quality: "ebook" },       // plain form fields (typed by the manifest)
//     title: "my-job",
//   });
//   AIO.watch(job.id, (snapshot) => { ... });   // optional: the Jobs panel already tracks it
//   const done = await AIO.call("other-plugin", { fields: {...} });   // run + wait
//   AIO.on("job.done", (ev) => { ... });         // server events (SSE)
//   await AIO.emit("my-plugin.saved", { n: 1 }); // publish a custom event
//   fetch(AIO.serviceUrl() + "api/thing");       // this plugin's background service
(() => {
  "use strict";
  const m = location.pathname.match(/^\/p\/([^/]+)\//);
  const pluginId = m ? decodeURIComponent(m[1]) : null;
  const inFrame = window.parent !== window;
  const post = (msg) => inFrame && window.parent.postMessage(msg, location.origin);

  function submit({ files = {}, params, fields = {}, title, plugin = pluginId } = {}) {
    const fd = new FormData();
    for (const [k, v] of Object.entries(fields)) fd.append(k, typeof v === "boolean" ? String(v) : v);
    if (params) fd.append("__params", JSON.stringify(params));
    if (title) fd.append("__title", title);
    let total = 0;
    for (const [k, list] of Object.entries(files)) for (const f of list) { fd.append(k, f, f.name); total += f.size; }
    const tmp = "upload-" + Math.random().toString(36).slice(2);

    return new Promise((resolve, reject) => {
      const xhr = new XMLHttpRequest();
      xhr.open("POST", `/api/plugins/${encodeURIComponent(plugin)}/run`);
      xhr.upload.onprogress = (e) => {
        if (total && e.lengthComputable) post({ type: "aio:upload", id: tmp, plugin, progress: (e.loaded / e.total) * 100, status: "Uploading…" });
      };
      xhr.onload = () => {
        post({ type: "aio:upload", id: tmp, done: true });
        let body = {};
        try { body = JSON.parse(xhr.responseText); } catch {}
        if (xhr.status >= 300) return reject(new Error(body.error || xhr.statusText));
        post({ type: "aio:job", job: body });
        resolve(body);
      };
      xhr.onerror = () => { post({ type: "aio:upload", id: tmp, done: true }); reject(new Error("Network error")); };
      xhr.send(fd);
    });
  }

  function watch(id, cb) {
    const es = new EventSource(`/api/jobs/${id}/events`);
    const on = (e) => {
      const j = JSON.parse(e.data);
      cb(j);
      if (["done", "failed", "canceled"].includes(j.state)) es.close();
    };
    es.addEventListener("snapshot", on);
    es.addEventListener("update", on);
    return () => es.close();
  }

  // Run a plugin and resolve with the finished job (rejects if it fails).
  //   const job = await AIO.call("pdf-merge", { files: { files: [a, b] } });
  //   job.files[0].url
  async function call(plugin, opts = {}) {
    const job = await submit({ ...opts, plugin });
    const done = await new Promise((resolve) => {
      const stop = watch(job.id, (snap) => {
        if (["done", "failed", "canceled"].includes(snap.state)) { stop(); resolve(snap); }
      });
    });
    const full = await (await fetch(`/api/jobs/${done.id}`)).json();
    if (full.state !== "done") throw Object.assign(new Error(full.error || full.state), { job: full });
    return full;
  }

  // Plugins that declare `provides: [capability]`.
  async function find(capability) {
    const r = await fetch(`/api/plugins?provides=${encodeURIComponent(capability)}`);
    return (await r.json()).plugins;
  }

  // Subscribe to server events ("job.done", "plugin.*", "my-plugin.thing", "*").
  // Returns an unsubscribe function.
  function on(topic, cb) {
    const es = new EventSource(`/api/events?topic=${encodeURIComponent(topic)}`);
    es.onmessage = (e) => { try { cb(JSON.parse(e.data)); } catch (err) { console.error(err); } };
    return () => es.close();
  }

  async function emit(topic, data = {}) {
    const r = await fetch("/api/events", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ topic, data }) });
    if (!r.ok) throw new Error((await r.json().catch(() => ({}))).error || r.statusText);
  }

  // Base URL of a plugin's background service (default: this plugin's).
  const serviceUrl = (id = pluginId) => `/s/${encodeURIComponent(id)}/`;

  const fileUrl = (job, name) => `/api/jobs/${job.id || job}/files/${name.split("/").map(encodeURIComponent).join("/")}`;

  // Keep the parent iframe sized to the content.
  if (inFrame) {
    // Measure the body at auto height so the frame size never feeds back into itself.
    const send = () => post({ type: "aio:height", height: Math.ceil(document.body.getBoundingClientRect().height) + 24 });
    const init = () => {
      document.documentElement.style.height = "auto";
      document.body.style.height = "auto";
      new ResizeObserver(send).observe(document.body);
      send();
    };
    if (document.body) init(); else document.addEventListener("DOMContentLoaded", init);
    // Inherit an explicit theme from the parent page if it has one.
    try {
      const t = window.parent.document.documentElement.dataset.theme;
      if (t) document.documentElement.dataset.theme = t;
    } catch {}
  }

  window.AIO = { pluginId, submit, watch, fileUrl, call, find, on, emit, serviceUrl };
})();
