import Alpine from "alpinejs";
import focus from "@alpinejs/focus";
window.Alpine = Alpine;
Alpine.plugin(focus);

function fetchCaddyVersions() {
  fetch(`${import.meta.env.VITE_API_URL}/api/caddy_versions`)
    .then((response) => response.json())
    .then((data) => {
      store.caddyVersions = data;
    });
}

function fetchCaddyPackages() {
  fetch(`${import.meta.env.VITE_API_URL}/api/packages`)
    .then((response) => response.json())
    .then((data) => {
      // For each package, add a new property called version
      data.result.forEach((p) => {
        p.version = "";
      });

      store.caddyPackages = data.result.sort(
        (a, b) => b.downloads - a.downloads
      );
    });
}

function isPackageSelected(p) {
  return store.selectedCaddyModules.some((m) => m.id === p.id);
}

function togglePackageSelection(p) {
  if (isPackageSelected(p)) {
    store.selectedCaddyModules = store.selectedCaddyModules.filter(
      (m) => m.id !== p.id
    );
  } else {
    store.selectedCaddyModules.push(p);
  }
}

function packageMatchesSearch(p) {
  if (store.searchQuery === "" || store.searchQuery === null) {
    return true;
  }
  for (const module of p.modules) {
    if (module.name.toLowerCase().includes(store.searchQuery.toLowerCase())) {
      return true;
    }
    if (module.docs.toLowerCase().includes(store.searchQuery.toLowerCase())) {
      return true;
    }
  }
  return p.path.toLowerCase().includes(store.searchQuery.toLowerCase());
}

function generateBuildURL(job_id) {
  // Take each of the selected packages and generate a URL similar to the following:
  // /api/download?p=selectedpackage1.repo&p=selectedpackage2.repo&p=selectedpackage3.repo
  // The URL should be encoded
  var [os, arch, arm = ""] = store.os.split("-");
  var queryString = new URLSearchParams();
  if (os) queryString.set("os", os);
  if (arch) queryString.set("arch", arch);
  if (arm) queryString.set("arm", arm);
  if (store.caddyVersion) queryString.set("caddy_version", store.caddyVersion);
  // Loop through the selected packages and add them to the query string
  store.selectedCaddyModules.forEach((p) => {
    var pkgString = p.path;
    if (p.version) {
      pkgString = pkgString + "@" + p.version;
    }
    queryString.append("p", pkgString);
  });
  queryString.set("job_id", job_id);
  return `${
    import.meta.env.VITE_API_URL
  }/api/download?${queryString.toString()}`;
}

function submitBuild() {
  store.buildLog = [];
  addBuildLog("Waiting for build to start...");
  const job_id = generateBuildGUID();
  const buildURL = generateBuildURL(job_id);
  store.buildURL = buildURL;
  store.buildStatus = "pending";
  connectToWebSocket(job_id);

  // Wait 500 milliseconds and then submit the build
  setTimeout(() => {
    // Fetch the build URL (HEAD request)
    fetch(buildURL, { method: "HEAD" }).then((response) => {
      if (response.status === 200) {
        store.buildStatus = "success";
        window.location.href = buildURL;
        store.disconnectFromWebSocket();
      } else {
        addBuildLog("Build failed");
        store.buildStatus = "failed";
        store.disconnectFromWebSocket();
      }
    });
  }, 500);
}

function generateBuildGUID() {
  // I would love to use the random UUID, but its only available in secure contexts
  // and for local/self hosters, I can't guarantee that this will always be executed
  // in a secure context.
  // So, we'll use a simple GUID generator instead (credit StackOverflow, yeah, its still useful)
  return "10000000-1000-4000-8000-100000000000".replace(/[018]/g, (c) =>
    (
      +c ^
      (crypto.getRandomValues(new Uint8Array(1))[0] & (15 >> (+c / 4)))
    ).toString(16)
  );
}

function connectToWebSocket(job_id) {
  const ws = new WebSocket(
    `${import.meta.env.VITE_API_URL}/ws?job_id=${job_id}`
  );
  store.websocketConnection = ws;
  ws.onmessage = (event) => {
    const data = JSON.parse(event.data);
    if (data.message_type === "log") {
      addBuildLog(data.message);
    }
    if (data.message_type === "build_success") {
      addBuildLog(data.message);
      store.buildStatus = "success";
    }
    if (data.message_type === "build_failed") {
      addBuildLog(data.message);
      store.buildStatus = "failed";
    }
  };
  ws.onopen = () => {
    addBuildLog("Build started");
  };
  ws.onclose = () => {
    store.websocketConnection = null;
  };
}

function addBuildLog(message) {
  store.buildLog.push(message);
  const buildlog = document.getElementById("buildlog");
  // Scroll to the bottom of the buildlog
  buildlog.scrollTop = buildlog.scrollHeight;
}

function disconnectFromWebSocket() {
  if (store.websocketConnection) {
    store.websocketConnection.close();
    store.websocketConnection = null;
  }
}

function detectPlatform() {
  // assume 32-bit linux, then change OS and architecture if justified
  var os = "linux",
    arch = "amd64";

  // change os
  if (/Macintosh/i.test(navigator.userAgent)) {
    os = "darwin";
  } else if (/Windows/i.test(navigator.userAgent)) {
    os = "windows";
  } else if (/FreeBSD/i.test(navigator.userAgent)) {
    os = "freebsd";
  } else if (/OpenBSD/i.test(navigator.userAgent)) {
    os = "openbsd";
  }

  // change architecture
  if (
    os == "darwin" ||
    /amd64|x64|x86_64|Win64|WOW64|i686|64-bit/i.test(navigator.userAgent)
  ) {
    arch = "amd64";
  } else if (/arm64/.test(navigator.userAgent)) {
    arch = "arm64";
  } else if (/ ARM| armv/.test(navigator.userAgent)) {
    arch = "arm";
  }

  // change arm version
  if (arch == "arm") {
    var arm = "7"; // assume version 7 by default
    if (/armv6/.test(navigator.userAgent)) {
      arm = "6";
    } else if (/armv5/.test(navigator.userAgent)) {
      arm = "5";
    }
    arch += arm;
  }

  return [os, arch];
}

function copyDownloadLink() {
  // If the buildURL doesn't start with http: or https:, then add the current domain
  if (
    !store.buildURL.startsWith("http:") &&
    !store.buildURL.startsWith("https:")
  ) {
    store.buildURL = `${window.location.origin}${store.buildURL}`;
  }
  navigator.clipboard.writeText(store.buildURL);
  store.copyLinkText = "Copied!";
  setTimeout(() => {
    store.copyLinkText = "Copy Download Link";
  }, 2000);
}

const store = Alpine.reactive({
  os: "linux-amd64",
  caddyVersion: "latest",
  caddyPackages: [],
  caddyVersions: [],
  selectedCaddyModules: [],
  buildLog: [],
  websocketConnection: null,
  buildURL: "",
  buildStatus: "pending",
  copyLinkText: "Copy Download Link",
  searchQuery: "",
  packageMatchesSearch: packageMatchesSearch,
  isPackageSelected: isPackageSelected,
  togglePackageSelection: togglePackageSelection,
  generateBuildURL: generateBuildURL,
  submitBuild: submitBuild,
  disconnectFromWebSocket: disconnectFromWebSocket,
  copyDownloadLink: copyDownloadLink,
});

document.addEventListener("alpine:init", () => {
  fetchCaddyVersions();
  fetchCaddyPackages();
  const [os, arch] = detectPlatform();
  store.os = `${os}-${arch}`;
  Alpine.store("main", store);
});

Alpine.start();
