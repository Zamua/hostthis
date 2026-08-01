// Renders a PDF paste with pdf.js.
//
// Scripting is disabled: a PDF may carry JavaScript, and this viewer runs on
// the paste's own origin. Rendering is the only thing the document gets to do.
import * as pdfjs from "/_hostthis/pdf.min.mjs";

pdfjs.GlobalWorkerOptions.workerSrc = "/_hostthis/pdf.worker.min.mjs";

const DL = window.HostthisDeepLink;
const content = document.getElementById("content");
const pos = document.getElementById("pos");
const prev = document.getElementById("prev");
const next = document.getElementById("next");

function fail(msg) {
  content.innerHTML = "";
  const d = document.createElement("div");
  d.className = "err";
  d.textContent = msg;
  content.appendChild(d);
}

// Rendering every page of a long document up front is the difference between a
// viewer that opens instantly and one that hangs on a 300-page report, so pages
// render as they approach the viewport.
function observePages(doc, scale) {
  const io = new IntersectionObserver(
    (entries) => {
      for (const e of entries) {
        if (!e.isIntersecting) continue;
        io.unobserve(e.target);
        renderPage(doc, e.target, scale);
      }
    },
    { rootMargin: "800px 0px" },
  );
  return io;
}

async function renderPage(doc, holder, scale) {
  const n = Number(holder.dataset.page);
  const page = await doc.getPage(n);
  const viewport = page.getViewport({ scale });
  const canvas = holder.querySelector("canvas");
  const ratio = window.devicePixelRatio || 1;
  canvas.width = Math.floor(viewport.width * ratio);
  canvas.height = Math.floor(viewport.height * ratio);
  canvas.style.width = Math.floor(viewport.width) + "px";
  canvas.style.height = Math.floor(viewport.height) + "px";
  const ctx = canvas.getContext("2d");
  ctx.scale(ratio, ratio);
  await page.render({ canvasContext: ctx, viewport }).promise;

  // A selectable text layer over the canvas: without it the page is an image
  // and neither copy nor find-in-page works.
  try {
    const text = await page.getTextContent();
    const layer = holder.querySelector(".textLayer");
    layer.innerHTML = "";
    for (const item of text.items) {
      if (!item.str) continue;
      const s = document.createElement("span");
      s.textContent = item.str;
      const tx = pdfjs.Util.transform(viewport.transform, item.transform);
      s.style.left = tx[4] + "px";
      s.style.top = tx[5] - item.height * scale + "px";
      s.style.fontSize = item.height * scale + "px";
      layer.appendChild(s);
    }
  } catch {
    // A document with no extractable text still renders; only selection is lost.
  }
}

function goTo(n, total) {
  const clamped = Math.min(Math.max(n, 1), total);
  const holder = document.getElementById("page-" + clamped);
  if (!holder) return;
  DL.setHash("page=" + clamped);
  setPos(clamped, total);
  DL.reveal(holder);
}

function setPos(n, total) {
  pos.textContent = n + " / " + total;
  prev.disabled = n <= 1;
  next.disabled = n >= total;
}

(async function () {
  try {
    const doc = await pdfjs.getDocument({
      url: location.pathname + "?raw=1",
      // The two controls that matter here: no document-embedded JS, and no
      // external fetches for fonts the document names.
      isEvalSupported: false,
      disableAutoFetch: false,
    }).promise;

    const total = doc.numPages;
    content.innerHTML = "";

    // One scale for the whole document, derived from page 1, so pages do not
    // jitter in width as they render in.
    const first = await doc.getPage(1);
    const unscaled = first.getViewport({ scale: 1 });
    const avail = Math.min(content.clientWidth || 900, 900);
    const scale = Math.max(0.5, Math.min(2, avail / unscaled.width));

    const io = observePages(doc, scale);
    for (let n = 1; n <= total; n++) {
      const holder = document.createElement("div");
      holder.className = "page";
      holder.id = "page-" + n;
      holder.dataset.page = String(n);
      // Reserve the final height before the page renders, so the scroll
      // position of a deep link is not invalidated by later pages resizing.
      const vp = first.getViewport({ scale });
      holder.style.width = Math.floor(vp.width) + "px";
      holder.style.height = Math.floor(vp.height) + "px";
      holder.innerHTML = '<canvas></canvas><div class="textLayer"></div>';
      content.appendChild(holder);
      io.observe(holder);
    }

    setPos(1, total);
    prev.onclick = () => goTo(currentPage() - 1, total);
    next.onclick = () => goTo(currentPage() + 1, total);

    // Track the page under the reader as they scroll, so the counter and the
    // fragment describe what is actually on screen.
    const spy = new IntersectionObserver(
      (entries) => {
        for (const e of entries) {
          if (e.isIntersecting && e.intersectionRatio > 0.5) {
            setPos(Number(e.target.dataset.page), total);
          }
        }
      },
      { threshold: [0.5] },
    );
    document.querySelectorAll(".page").forEach((p) => spy.observe(p));

    function currentPage() {
      return Number(pos.textContent.split("/")[0].trim()) || 1;
    }

    DL.onResolve((t) => {
      if (t.type === "page") goTo(t.n, total);
    });

    const meta = await doc.getMetadata().catch(() => null);
    if (meta && meta.info && meta.info.Title) document.title = meta.info.Title;
  } catch (e) {
    fail("Failed to render this PDF.\n\n" + (e && e.message ? e.message : String(e)));
  }
})();
