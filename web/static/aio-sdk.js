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

  window.AIO = { pluginId, submit, watch, fileUrl };
})();
