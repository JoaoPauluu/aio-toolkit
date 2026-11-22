// AIO Toolkit front-end: renders tools from plugin manifests and tracks jobs.
(() => {
  "use strict";

  const $ = (sel, root = document) => root.querySelector(sel);
  const el = (tag, attrs = {}, ...kids) => {
    const e = document.createElement(tag);
    for (const [k, v] of Object.entries(attrs)) {
      if (v == null || v === false) continue;
      if (k === "class") e.className = v;
      else if (k.startsWith("on")) e.addEventListener(k.slice(2), v);
      else if (k === "html") e.innerHTML = v;
      else e.setAttribute(k, v === true ? "" : v);
    }
    for (const kid of kids.flat()) if (kid != null && kid !== false) e.append(kid.nodeType ? kid : String(kid));
    return e;
  };
  const fmtSize = (n) => {
    if (n == null) return "";
    const u = ["B", "KB", "MB", "GB", "TB"];
    let i = 0;
    while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
    return `${n.toFixed(i && n < 10 ? 1 : 0)} ${u[i]}`;
  };
  const api = async (path, opts = {}) => {
    const r = await fetch(path, opts);
    const body = await r.json().catch(() => ({}));
    if (!r.ok) throw new Error(body.error || r.statusText);
    return body;
  };

  const state = {
    plugins: [],
    pluginsKey: "",
    jobs: new Map(),     // id -> snapshot (+ logs)
    sources: new Map(),  // id -> EventSource
    openLogs: new Set(),
    current: null,
  };

  // ---------------------------------------------------------------- plugins
  async function loadPlugins(force) {
    const data = await api(force ? "/api/plugins/reload" : "/api/plugins", force ? { method: "POST" } : {});
    const key = JSON.stringify(data);
    if (key === state.pluginsKey && !force) return;
    state.pluginsKey = key;
    state.plugins = data.plugins;
    renderNav();
    const errs = $("#plugin-errors");
    errs.replaceChildren(...data.errors.map((e) => el("div", { title: e.dir }, "⚠ " + e.error)));
    // re-render the current view only if the open plugin's manifest changed
    const cur = state.current && state.plugins.find((p) => p.id === state.current.id);
    if (!state.current || !cur || JSON.stringify(cur) !== JSON.stringify(state.current)) route();
  }

  function renderNav() {
    const q = $("#search").value.trim().toLowerCase();
    const nav = $("#nav");
    nav.replaceChildren();
    let cat = null;
    for (const p of state.plugins) {
      if (q && !(`${p.name} ${p.description} ${p.category}`.toLowerCase().includes(q))) continue;
      if (p.category !== cat) { cat = p.category; nav.append(el("div", { class: "nav-cat" }, cat)); }
      nav.append(el("a", {
        class: "nav-item" + (state.current?.id === p.id ? " active" : ""),
        href: `#/t/${p.id}`, title: p.description,
      }, el("span", { class: "nav-icon" }, p.icon || "🔧"), p.name));
    }
  }

  // ---------------------------------------------------------------- routing
  function route() {
    document.body.classList.remove("menu-open");
    const m = location.hash.match(/^#\/t\/([^/?]+)/);
    const p = m && state.plugins.find((x) => x.id === decodeURIComponent(m[1]));
    state.current = p || null;
    renderNav();
    $("#topbar-title").textContent = p ? p.name : "AIO Toolkit";
    document.title = p ? `${p.name} · AIO Toolkit` : "AIO Toolkit";
    const view = $("#view");
    if (!p) return renderHome(view);
    view.replaceChildren(
      el("div", { class: "tool-head" },
        el("div", { class: "ic" }, p.icon || "🔧"),
        el("div", {}, el("h1", {}, p.name), el("div", { class: "muted" }, p.description || ""))),
      p.ui_url ? renderCustomUI(p) : renderForm(p),
    );
  }

  function renderHome(view) {
    const byCat = new Map();
    for (const p of state.plugins) {
      if (!byCat.has(p.category)) byCat.set(p.category, []);
      byCat.get(p.category).push(p);
    }
    view.replaceChildren(
      el("h1", {}, "🧰 AIO Toolkit"),
      el("div", { class: "muted" }, `${state.plugins.length} tools loaded. Drop a folder with a plugin.yaml into the plugins directory to add more.`),
      ...[...byCat].flatMap(([cat, list]) => [
        el("h3", { class: "cat-title" }, cat),
        el("div", { class: "tool-grid" }, list.map((p) =>
          el("a", { class: "card tool-card", href: `#/t/${p.id}` },
            el("div", { class: "ic" }, p.icon || "🔧"),
            el("div", { class: "nm" }, p.name),
            el("div", { class: "ds" }, p.description || "")))),
      ]),
    );
  }

  function renderCustomUI(p) {
    return el("iframe", { class: "plugin-ui", src: p.ui_url, title: p.name, "data-plugin": p.id });
  }

  // ---------------------------------------------------------------- forms
  function renderForm(p) {
    const form = el("form", { class: "card tool-form", novalidate: true });
    const fileStore = {}; // input name -> File[]
    const fields = [];

    for (const inp of p.inputs) {
      if (inp.type === "json") continue; // structured data: set by custom UIs and API callers only
      const id = `f_${inp.name}`;
      let control;
      const label = el("label", { class: "lbl", for: id }, inp.label, inp.required ? el("span", { class: "req" }, " *") : null);
      const wrap = el("div", { class: "field", "data-name": inp.name });

      switch (inp.type) {
        case "file": {
          fileStore[inp.name] = [];
          const input = el("input", { type: "file", id, accept: inp.accept || null, multiple: inp.multiple });
          const list = el("ul", { class: "file-list" });
          const refresh = () => list.replaceChildren(...fileStore[inp.name].map((f, i) =>
            el("li", {}, el("span", { class: "nm" }, f.name),
              el("span", {}, el("span", { class: "muted small" }, fmtSize(f.size) + " "),
                el("button", { type: "button", class: "btn ghost small", onclick: () => { fileStore[inp.name].splice(i, 1); refresh(); } }, "✕")))));
          const add = (files) => {
            const arr = [...files];
            fileStore[inp.name] = inp.multiple ? fileStore[inp.name].concat(arr) : arr.slice(0, 1);
            refresh();
          };
          input.addEventListener("change", () => { add(input.files); input.value = ""; });
          const dz = el("label", { class: "dropzone", for: id },
            input, el("div", {}, "📂 Drop file" + (inp.multiple ? "s" : "") + " here or click to browse"),
            inp.accept ? el("div", { class: "small" }, inp.accept) : null);
          dz.addEventListener("dragover", (e) => { e.preventDefault(); dz.classList.add("over"); });
          dz.addEventListener("dragleave", () => dz.classList.remove("over"));
          dz.addEventListener("drop", (e) => { e.preventDefault(); dz.classList.remove("over"); add(e.dataTransfer.files); });
          wrap.append(label, dz, list);
          break;
        }
        case "checkbox":
          control = el("input", { type: "checkbox", id, name: inp.name, checked: !!inp.default });
          wrap.append(el("label", { class: "check" }, control, el("span", {}, inp.label)));
          break;
        case "select":
          control = el("select", { id, name: inp.name },
            (inp.options || []).map((o) => el("option", { value: o.value, selected: String(inp.default) === o.value }, o.label)));
          wrap.append(label, control);
          break;
        case "textarea":
          control = el("textarea", { id, name: inp.name, placeholder: inp.placeholder || null });
          control.value = inp.default ?? "";
          wrap.append(label, control);
          break;
        case "hidden":
          control = el("input", { type: "hidden", name: inp.name, value: inp.default ?? "" });
          wrap.hidden = true;
          wrap.append(control);
          break;
        default:
          control = el("input", {
            type: inp.type === "number" ? "number" : inp.type === "url" ? "url" : "text",
            id, name: inp.name, placeholder: inp.placeholder || null,
            min: inp.min, max: inp.max, step: inp.step ?? (inp.type === "number" ? "any" : null),
          });
          control.value = inp.default ?? "";
          wrap.append(label, control);
      }
      if (inp.help) wrap.append(el("div", { class: "help" }, inp.help));
      form.append(wrap);
      fields.push({ inp, wrap, control });
    }

    const applyShowIf = () => {
      const val = (name) => {
        const f = fields.find((x) => x.inp.name === name);
        if (!f?.control) return "";
        return f.control.type === "checkbox" ? String(f.control.checked) : f.control.value;
      };
      for (const f of fields) {
        if (!f.inp.show_if || f.inp.type === "hidden") continue;
        f.wrap.hidden = !Object.entries(f.inp.show_if).every(([k, vals]) => vals.map(String).includes(val(k)));
      }
    };
    form.addEventListener("input", applyShowIf);
    form.addEventListener("change", applyShowIf);
    applyShowIf();

    const btn = el("button", { class: "btn primary", type: "submit" }, p.submit_label || "Run");
    const msg = el("span", { class: "job-err" });
    form.append(el("div", { class: "form-actions" }, btn, msg));

    form.addEventListener("submit", async (e) => {
      e.preventDefault();
      msg.textContent = "";
      for (const { inp, wrap, control } of fields) {
        if (!inp.required || wrap.hidden) continue;
        const empty = inp.type === "file" ? !fileStore[inp.name].length : !control.value.trim();
        if (empty) { msg.textContent = `${inp.label} is required`; return; }
      }
      const fd = new FormData();
      for (const { inp, wrap, control } of fields) {
        if (inp.type === "file") fileStore[inp.name].forEach((f) => fd.append(inp.name, f, f.name));
        else if (inp.type === "checkbox") fd.append(inp.name, control.checked ? "true" : "false");
        else if (!wrap.hidden || inp.type === "hidden") fd.append(inp.name, control.value);
      }
      btn.disabled = true;
      try {
        await submitJob(p, fd);
      } catch (err) {
        msg.textContent = err.message;
      } finally {
        btn.disabled = false;
      }
    });
    return form;
  }

  // Upload with progress (fetch has no upload progress), shown as a temporary job card.
  function submitJob(p, fd) {
    return new Promise((resolve, reject) => {
      const tmpId = "upload-" + Math.random().toString(36).slice(2);
      const hasFiles = [...fd.values()].some((v) => v instanceof File);
      if (hasFiles) setJob({ id: tmpId, plugin: p.id, plugin_name: p.name, state: "uploading", progress: 0, status: "Uploading…", files: [], created: new Date().toISOString() });
      const xhr = new XMLHttpRequest();
      xhr.open("POST", `/api/plugins/${encodeURIComponent(p.id)}/run`);
      xhr.upload.onprogress = (e) => {
        if (hasFiles && e.lengthComputable) setJob({ ...state.jobs.get(tmpId), progress: (e.loaded / e.total) * 100, status: `Uploading… ${fmtSize(e.loaded)} / ${fmtSize(e.total)}` });
      };
      xhr.onload = () => {
        removeJob(tmpId);
        let body = {};
        try { body = JSON.parse(xhr.responseText); } catch {}
        if (xhr.status >= 300) return reject(new Error(body.error || xhr.statusText));
        trackJob(body);
        resolve(body);
      };
      xhr.onerror = () => { removeJob(tmpId); reject(new Error("Network error")); };
      xhr.send(fd);
    });
  }

  // ---------------------------------------------------------------- jobs
  function setJob(j) {
    const prev = state.jobs.get(j.id);
    state.jobs.set(j.id, { ...prev, ...j, logs: j.logs ?? prev?.logs ?? [] });
    renderJobs();
  }
  function removeJob(id) {
    state.jobs.delete(id);
    state.sources.get(id)?.close();
    state.sources.delete(id);
    renderJobs();
  }

  function trackJob(j) {
    setJob(j);
    if (window.innerWidth <= 1100) document.body.classList.add("jobs-open");
    requestAnimationFrame(() => document.querySelector(`[data-job="${j.id}"]`)?.classList.add("flash"));
    watch(j.id);
  }

  function watch(id) {
    if (state.sources.has(id)) return;
    const es = new EventSource(`/api/jobs/${id}/events`);
    state.sources.set(id, es);
    const done = (j) => {
      if (["done", "failed", "canceled"].includes(j.state)) { es.close(); state.sources.delete(id); }
    };
    es.addEventListener("snapshot", (e) => { const j = JSON.parse(e.data); setJob(j); done(j); });
    es.addEventListener("update", (e) => { const j = JSON.parse(e.data); setJob(j); done(j); });
    es.addEventListener("log", (e) => {
      const j = state.jobs.get(id);
      if (!j) return;
      j.logs.push(JSON.parse(e.data));
      if (j.logs.length > 2000) j.logs.splice(0, 1000);
      scheduleRender();
    });
    es.onerror = () => {
      // Job deleted or server restarted: stop retrying once we know it's gone.
      fetch(`/api/jobs/${id}`).then((r) => { if (r.status === 404) removeJob(id); });
    };
  }

  let renderPending = false;
  function scheduleRender() {
    if (renderPending) return;
    renderPending = true;
    requestAnimationFrame(() => { renderPending = false; renderJobs(); });
  }

  function renderJobs() {
    const list = $("#job-list");
    const jobs = [...state.jobs.values()].sort((a, b) => (b.created > a.created ? 1 : -1));
    const active = jobs.filter((j) => ["queued", "running", "uploading"].includes(j.state)).length;
    document.querySelectorAll(".jobs-toggle .badge").forEach((b) => { b.hidden = !active; b.textContent = active; });
    if (!jobs.length) return list.replaceChildren(el("p", { class: "muted empty" }, "No jobs yet."));

    // Preserve log scroll positions across re-renders.
    const scroll = {};
    list.querySelectorAll("pre.logs").forEach((pre) => {
      scroll[pre.dataset.id] = pre.scrollHeight - pre.scrollTop - pre.clientHeight < 8 ? "bottom" : pre.scrollTop;
    });

    list.replaceChildren(...jobs.map((j) => {
      const finished = ["done", "failed", "canceled"].includes(j.state);
      const live = !finished;
      const pct = j.progress >= 0 ? j.progress : null;
      const card = el("div", { class: "card job", "data-job": j.id },
        el("div", { class: "job-top" },
          el("span", { class: "job-name" }, j.plugin_name || j.plugin),
          el("span", { class: `state ${j.state}` }, j.state)),
        j.title ? el("div", { class: "job-title", title: j.title }, j.title) : null,
        live ? el("div", { class: "bar" + (pct == null ? " indet" : "") }, el("div", { style: `width:${pct ?? 0}%` })) : null,
        j.status && !(finished && j.state !== "done" && j.error) ? el("div", { class: "job-status" }, live && pct != null ? `${pct.toFixed(0)}% · ${j.status}` : j.status) : null,
        j.error && j.state !== "done" ? el("div", { class: "job-err" }, j.error) : null,
        j.files?.length ? el("ul", { class: "job-files" }, j.files.map((f) =>
          el("li", {}, el("a", { href: `/api/jobs/${j.id}/files/${f.name.split("/").map(encodeURIComponent).join("/")}`, title: f.name }, f.name),
            el("span", { class: "muted small" }, fmtSize(f.size))))) : null,
      );
      if (j.state !== "uploading") {
        const actions = el("div", { class: "job-actions" });
        if (j.files?.length > 1) actions.append(el("a", { class: "btn small", href: `/api/jobs/${j.id}/zip` }, "⬇ All (zip)"));
        if (live) actions.append(el("button", { class: "btn small danger", onclick: () => api(`/api/jobs/${j.id}/cancel`, { method: "POST" }).catch(alert) }, "Cancel"));
        actions.append(el("button", {
          class: "btn small ghost", onclick: () => { state.openLogs.has(j.id) ? state.openLogs.delete(j.id) : state.openLogs.add(j.id); renderJobs(); },
        }, state.openLogs.has(j.id) ? "Hide log" : `Log (${j.logs?.length || 0})`));
        if (finished) actions.append(el("button", {
          class: "btn small ghost", title: "Delete job and its files",
          onclick: () => api(`/api/jobs/${j.id}`, { method: "DELETE" }).then(() => removeJob(j.id)).catch(alert),
        }, "🗑"));
        card.append(actions);
        if (state.openLogs.has(j.id)) card.append(el("pre", { class: "logs", "data-id": j.id }, (j.logs || []).join("\n") || "(empty)"));
      }
      return card;
    }));

    list.querySelectorAll("pre.logs").forEach((pre) => {
      const s = scroll[pre.dataset.id];
      pre.scrollTop = s === undefined || s === "bottom" ? pre.scrollHeight : s;
    });
  }

  async function loadJobs() {
    const { jobs } = await api("/api/jobs");
    for (const j of jobs) {
      setJob(j);
      if (!["done", "failed", "canceled"].includes(j.state)) watch(j.id);
    }
  }

  // Fetch logs lazily when a finished job's log is opened.
  const origToggle = state.openLogs.add.bind(state.openLogs);
  state.openLogs.add = (id) => {
    const j = state.jobs.get(id);
    if (j && !state.sources.has(id) && !j._logsLoaded) {
      api(`/api/jobs/${id}`).then((full) => { full._logsLoaded = true; setJob(full); }).catch(() => {});
    }
    return origToggle(id);
  };

  // ---------------------------------------------------------------- custom UI bridge
  window.addEventListener("message", (e) => {
    if (e.origin !== location.origin || !e.data || typeof e.data !== "object") return;
    const frame = [...document.querySelectorAll("iframe.plugin-ui")].find((f) => f.contentWindow === e.source);
    if (!frame) return;
    if (e.data.type === "aio:job") trackJob(e.data.job);
    if (e.data.type === "aio:upload") {
      const { id, plugin, progress, status, done } = e.data;
      if (done) removeJob(id);
      else setJob({ id, plugin, plugin_name: state.plugins.find((p) => p.id === plugin)?.name, state: "uploading", progress, status, files: [], created: new Date().toISOString() });
    }
    if (e.data.type === "aio:height") frame.style.height = Math.max(300, e.data.height + 4) + "px";
  });

  // ---------------------------------------------------------------- wiring
  $("#search").addEventListener("input", renderNav);
  $("#reload").addEventListener("click", () => loadPlugins(true).catch(alert));
  $("#menu-open").addEventListener("click", () => document.body.classList.add("menu-open"));
  $("#menu-close").addEventListener("click", () => document.body.classList.remove("menu-open"));
  document.querySelectorAll(".jobs-toggle").forEach((b) => b.addEventListener("click", () => document.body.classList.toggle("jobs-open")));
  $("#clear-done").addEventListener("click", async () => {
    for (const j of [...state.jobs.values()]) {
      if (["done", "failed", "canceled"].includes(j.state)) {
        await api(`/api/jobs/${j.id}`, { method: "DELETE" }).catch(() => {});
        removeJob(j.id);
      }
    }
  });
  window.addEventListener("hashchange", route);
  window.addEventListener("focus", () => loadPlugins().catch(() => {}));
  setInterval(() => { if (!document.hidden) loadPlugins().catch(() => {}); }, 15000);

  loadPlugins().catch((e) => { $("#view").textContent = "Failed to load plugins: " + e.message; });
  loadJobs().catch(() => {});
})();
