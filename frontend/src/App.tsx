import { ChangeEvent, FormEvent, useEffect, useMemo, useRef, useState } from "react";
import type { DrivePreviewFile, Job, UploadReviewFile, User } from "./types";

const apiBaseURL = import.meta.env.VITE_API_BASE_URL ?? "http://localhost:8080/api";
const tokenStorageKey = "merge-pdf-token";
type TabKey = "drive" | "upload" | "history";
const uploadAccept = "application/pdf,image/png,.pdf,.png";

function App() {
  const [token, setToken] = useState<string>(() => localStorage.getItem(tokenStorageKey) ?? "");
  const [user, setUser] = useState<User | null>(null);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [activeTab, setActiveTab] = useState<TabKey>(() => getTabFromHash(window.location.hash));
  const [loading, setLoading] = useState(false);

  const [loginEmail, setLoginEmail] = useState("user@example.com");
  const [loginPassword, setLoginPassword] = useState("ChangeMe123!");

  const [driveURL, setDriveURL] = useState("");
  const [driveFiles, setDriveFiles] = useState<DrivePreviewFile[]>([]);

  const [uploadFiles, setUploadFiles] = useState<UploadReviewFile[]>([]);

  const [jobs, setJobs] = useState<Job[]>([]);
  const [selectedJob, setSelectedJob] = useState<Job | null>(null);
  const [activeJob, setActiveJob] = useState<Job | null>(null);
  const [downloadSpeedBps, setDownloadSpeedBps] = useState<number | null>(null);
  const downloadSampleRef = useRef<{ fileName: string; bytes: number; timestamp: number } | null>(null);

  useEffect(() => {
    if (!token) {
      setUser(null);
      setJobs([]);
      setSelectedJob(null);
      return;
    }

    void bootstrap();
  }, [token]);

  useEffect(() => {
    function syncTabFromHash() {
      setActiveTab(getTabFromHash(window.location.hash));
    }

    window.addEventListener("hashchange", syncTabFromHash);
    syncTabFromHash();
    return () => window.removeEventListener("hashchange", syncTabFromHash);
  }, []);

  useEffect(() => {
    if (!token || !activeJob || (activeJob.status !== "pending" && activeJob.status !== "running")) {
      return;
    }

    const interval = window.setInterval(() => {
      void pollJob(activeJob.id);
    }, 1200);

    return () => window.clearInterval(interval);
  }, [token, activeJob]);

  useEffect(() => {
    if (window.location.hash !== tabToHash(activeTab)) {
      window.history.replaceState(null, "", tabToHash(activeTab));
    }
  }, [activeTab]);

  useEffect(() => {
    if (!activeJob || activeJob.currentStage !== "downloading" || !activeJob.currentFileName) {
      downloadSampleRef.current = null;
      setDownloadSpeedBps(null);
      return;
    }

    const now = Date.now();
    const currentBytes = activeJob.currentFileBytes ?? 0;
    const sample = downloadSampleRef.current;
    if (sample && sample.fileName === activeJob.currentFileName) {
      const elapsedSeconds = (now - sample.timestamp) / 1000;
      if (elapsedSeconds > 0 && currentBytes >= sample.bytes) {
        setDownloadSpeedBps((currentBytes - sample.bytes) / elapsedSeconds);
      }
    } else {
      setDownloadSpeedBps(null);
    }

    downloadSampleRef.current = {
      fileName: activeJob.currentFileName,
      bytes: currentBytes,
      timestamp: now
    };
  }, [activeJob]);

  const sortedUploadFiles = useMemo(
    () => [...uploadFiles].sort((a, b) => a.order - b.order || a.file.name.localeCompare(b.file.name)),
    [uploadFiles]
  );
  const sortedDriveFiles = useMemo(
    () => [...driveFiles].sort((a, b) => a.extractedOrder - b.extractedOrder || a.name.localeCompare(b.name)),
    [driveFiles]
  );

  async function bootstrap() {
    try {
      const currentUser = await api<User>("/me", { token });
      setUser(currentUser);
      const payload = await api<{ jobs: Job[] }>("/jobs", { token });
      const nextJobs = ensureArray(payload.jobs);
      setJobs(nextJobs);
      const runningJob = nextJobs.find((job) => job.status === "pending" || job.status === "running");
      if (runningJob) {
        setActiveJob(runningJob);
      }
    } catch (err) {
      localStorage.removeItem(tokenStorageKey);
      setToken("");
      setError(getErrorMessage(err));
    }
  }

  async function handleLogin(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setLoading(true);
    setError("");
    try {
      const payload = await api<{ token: string; user: User }>("/auth/login", {
        method: "POST",
        body: JSON.stringify({ email: loginEmail, password: loginPassword })
      });
      localStorage.setItem(tokenStorageKey, payload.token);
      setToken(payload.token);
      setUser(payload.user);
      setNotice("Logged in successfully.");
    } catch (err) {
      setError(getErrorMessage(err));
    } finally {
      setLoading(false);
    }
  }

  async function handleLogout() {
    localStorage.removeItem(tokenStorageKey);
    setToken("");
    setUser(null);
    setNotice("Logged out.");
  }

  async function handleDrivePreview() {
    setLoading(true);
    setError("");
    try {
      const payload = await api<{ files: DrivePreviewFile[] }>("/drive/preview", {
        method: "POST",
        token,
        body: JSON.stringify({ url: driveURL })
      });
      const files = ensureArray(payload.files);
      setDriveFiles(files);
      setNotice(`Loaded ${files.length} Drive file(s) for merge.`);
    } catch (err) {
      setError(getErrorMessage(err));
    } finally {
      setLoading(false);
    }
  }

  async function handleDriveMerge() {
    setLoading(true);
    setError("");
    try {
      const job = await api<Job>("/merge/drive", {
        method: "POST",
        token,
        body: JSON.stringify({
          url: driveURL,
          orders: Object.fromEntries(driveFiles.map((file) => [file.sourceId, file.extractedOrder]))
        })
      });
      setActiveJob(job);
      setSelectedJob(job);
      navigateToTab("history");
      setNotice("Drive merge job started. Waiting for progress updates.");
    } catch (err) {
      setError(getErrorMessage(err));
    } finally {
      setLoading(false);
    }
  }

  async function handleUploadMerge() {
    setLoading(true);
    setError("");
    try {
      const formData = new FormData();
      const orders: Record<string, number> = {};
      sortedUploadFiles.forEach((item) => {
        formData.append("files", item.file);
        orders[item.file.name] = item.order;
      });
      formData.append("orders", JSON.stringify(orders));

      const response = await fetch(`${apiBaseURL}/merge/upload`, {
        method: "POST",
        headers: {
          Authorization: `Bearer ${token}`
        },
        body: formData
      });
      if (!response.ok) {
        throw new Error(await readError(response));
      }
      const job = (await response.json()) as Job;
      setActiveJob(job);
      setSelectedJob(job);
      setNotice("Upload merge job started. Waiting for progress updates.");
      setUploadFiles([]);
      navigateToTab("history");
    } catch (err) {
      setError(getErrorMessage(err));
    } finally {
      setLoading(false);
    }
  }

  async function refreshJobs() {
    const payload = await api<{ jobs: Job[] }>("/jobs", { token });
    setJobs(ensureArray(payload.jobs));
  }

  async function viewJob(jobId: number) {
    try {
      const payload = await api<Job>(`/jobs/${jobId}`, { token });
      setSelectedJob(payload);
      if (payload.status === "pending" || payload.status === "running") {
        setActiveJob(payload);
      }
    } catch (err) {
      setError(getErrorMessage(err));
    }
  }

  async function pollJob(jobId: number) {
    try {
      const payload = await api<Job>(`/jobs/${jobId}`, { token });
      setActiveJob(payload);
      setSelectedJob((current) => (current?.id === jobId ? payload : current));
      setJobs((current) => {
        const next = current.some((job) => job.id === payload.id)
          ? current.map((job) => (job.id === payload.id ? payload : job))
          : [payload, ...current];
        return next.sort((a, b) => new Date(b.createdAt).getTime() - new Date(a.createdAt).getTime());
      });

      if (payload.status === "completed") {
        setNotice(`Job ${payload.id} completed.`);
        setActiveJob(null);
      } else if (payload.status === "failed") {
        setError(payload.errorMessage ?? "Merge job failed.");
        setActiveJob(null);
      }
    } catch (err) {
      setError(getErrorMessage(err));
      setActiveJob(null);
    }
  }

  async function downloadJob(jobId: number, fileName: string) {
    try {
      const response = await fetch(`${apiBaseURL}/jobs/${jobId}/download`, {
        headers: { Authorization: `Bearer ${token}` }
      });
      if (!response.ok) {
        throw new Error(await readError(response));
      }
      await downloadResponse(response, fileName);
    } catch (err) {
      setError(getErrorMessage(err));
    }
  }

  async function deleteJob(jobId: number) {
    try {
      await api(`/jobs/${jobId}`, { method: "DELETE", token });
      if (selectedJob?.id === jobId) {
        setSelectedJob(null);
      }
      await refreshJobs();
      setNotice("Job deleted.");
    } catch (err) {
      setError(getErrorMessage(err));
    }
  }

  async function retryJob(jobId: number) {
    try {
      const job = await api<Job>(`/jobs/${jobId}/retry`, { method: "POST", token });
      setActiveJob(job);
      setSelectedJob(job);
      setJobs((current) => [job, ...current.filter((item) => item.id !== job.id)]);
      navigateToTab("history");
      setNotice(`Retry job ${job.id} started.`);
    } catch (err) {
      setError(getErrorMessage(err));
    }
  }

  function onFilesSelected(event: ChangeEvent<HTMLInputElement>) {
    const selected = Array.from(event.target.files ?? []).filter((file) => isSupportedUploadFile(file.name));
    setUploadFiles((current) => {
      const next = [...current];
      let nextOrder = current.length + 1;
      for (const file of selected) {
        const existingIndex = next.findIndex((item) => item.file.name === file.name);
        if (existingIndex >= 0) {
          next[existingIndex] = { ...next[existingIndex], file };
          continue;
        }
        next.push({
          file,
          order: nextOrder
        });
        nextOrder += 1;
      }
      return next;
    });
    event.target.value = "";
    setNotice(`Prepared ${selected.length} additional local file(s) for merge.`);
  }

  function updateUploadOrder(name: string, order: number) {
    if (!Number.isFinite(order) || order < 1) {
      return;
    }
    setUploadFiles((current) =>
      current.map((item) => (item.file.name === name ? { ...item, order } : item))
    );
  }

  function updateDriveOrder(sourceId: string, order: number) {
    if (!Number.isFinite(order) || order < 1) {
      return;
    }
    setDriveFiles((current) =>
      current.map((file) => (file.sourceId === sourceId ? { ...file, extractedOrder: order } : file))
    );
  }

  function moveDriveFile(sourceId: string, direction: "up" | "down") {
    setDriveFiles((current) => {
      const sorted = [...current].sort((a, b) => a.extractedOrder - b.extractedOrder || a.name.localeCompare(b.name));
      const index = sorted.findIndex((file) => file.sourceId === sourceId);
      if (index < 0) {
        return current;
      }

      const targetIndex = direction === "up" ? index - 1 : index + 1;
      if (targetIndex < 0 || targetIndex >= sorted.length) {
        return current;
      }

      const [moved] = sorted.splice(index, 1);
      sorted.splice(targetIndex, 0, moved);
      return sorted.map((file, orderIndex) => ({
        ...file,
        extractedOrder: orderIndex + 1
      }));
    });
  }

  function removeUploadFile(name: string) {
    setUploadFiles((current) => current.filter((item) => item.file.name !== name));
  }

  function navigateToTab(tab: TabKey) {
    window.location.hash = tabToHash(tab);
  }

  if (!token || !user) {
    return (
      <main className="shell shell-login">
        <section className="hero-card">
          <p className="eyebrow">Authenticated PDF workspace</p>
          <h1>Merge Drive folders or local PDFs without losing history.</h1>
          <p className="hero-copy">
            The app stores merged outputs in MinIO, keeps job history per account, and enforces Drive ordering from numeric prefixes in filenames.
          </p>
        </section>

        <section className="panel login-panel">
          <form onSubmit={handleLogin}>
            <label>
              Email
              <input value={loginEmail} onChange={(event) => setLoginEmail(event.target.value)} type="email" required />
            </label>
            <label>
              Password
              <input value={loginPassword} onChange={(event) => setLoginPassword(event.target.value)} type="password" required />
            </label>
            <button type="submit" disabled={loading}>
              {loading ? "Signing in..." : "Sign in"}
            </button>
          </form>
          <p className="hint">Seed accounts default to `user@example.com` / `ChangeMe123!`.</p>
          {error ? <p className="feedback error">{error}</p> : null}
          {notice ? <p className="feedback success">{notice}</p> : null}
        </section>
      </main>
    );
  }

  return (
    <main className="shell">
      <header className="topbar">
        <div>
          <p className="eyebrow">Merge PDF control room</p>
          <h1>{user.email}</h1>
        </div>
        <div className="topbar-actions">
          <span className="role-pill">{user.role}</span>
          <button className="ghost" onClick={handleLogout}>
            Log out
          </button>
        </div>
      </header>

      <nav className="tabs">
        <button className={activeTab === "drive" ? "active" : ""} onClick={() => navigateToTab("drive")}>Drive Link</button>
        <button className={activeTab === "upload" ? "active" : ""} onClick={() => navigateToTab("upload")}>Upload Files</button>
        <button className={activeTab === "history" ? "active" : ""} onClick={() => navigateToTab("history")}>History</button>
      </nav>

      {error ? <p className="feedback error">{error}</p> : null}
      {notice ? <p className="feedback success">{notice}</p> : null}
      {activeJob ? (
        <section className="progress-banner">
          <div className="progress-banner-copy">
            <strong>{activeJob.outputFilename}</strong>
            <span>
              {activeJob.status === "failed"
                ? activeJob.errorMessage ?? "Merge failed."
                : formatJobProgress(activeJob, downloadSpeedBps)}
            </span>
          </div>
          <div className="progress-track" aria-hidden="true">
            <div className="progress-fill" style={{ width: `${activeJob.progressPercent}%` }} />
          </div>
        </section>
      ) : null}

      {activeTab === "drive" ? (
        <section className="panel grid">
          <div className="stack">
            <label>
              Google Drive folder link
              <input
                value={driveURL}
                onChange={(event) => setDriveURL(event.target.value)}
                placeholder="https://drive.google.com/drive/u/0/folders/..."
              />
            </label>
            <div className="actions">
              <button onClick={handleDrivePreview} disabled={loading || !driveURL}>
                Preview Files
              </button>
              <button className="ghost" onClick={handleDriveMerge} disabled={loading || !driveFiles.length}>
                Merge by Filename Numbers
              </button>
            </div>
          </div>

          <div className="panel inset">
            <h2>Drive preview</h2>
            <div className="table-scroll">
              <table>
                <thead>
                  <tr>
                    <th>Order</th>
                    <th>Name</th>
                    <th>Size</th>
                    <th></th>
                  </tr>
                </thead>
                <tbody>
                  {sortedDriveFiles.length ? (
                    sortedDriveFiles.map((file, index) => (
                      <tr key={file.sourceId}>
                        <td>{index + 1}</td>
                        <td>
                          <a href={file.webViewLink} target="_blank" rel="noreferrer">
                            {file.name}
                          </a>
                        </td>
                        <td>{formatBytes(file.size)}</td>
                        <td>
                          <div className="row-actions">
                            <button
                              className="ghost compact icon-button"
                              onClick={() => moveDriveFile(file.sourceId, "up")}
                              disabled={index === 0}
                              aria-label={`Move ${file.name} up`}
                            >
                              ↑
                            </button>
                            <button
                              className="ghost compact icon-button"
                              onClick={() => moveDriveFile(file.sourceId, "down")}
                              disabled={index === sortedDriveFiles.length - 1}
                              aria-label={`Move ${file.name} down`}
                            >
                              ↓
                            </button>
                          </div>
                        </td>
                      </tr>
                    ))
                  ) : (
                    <tr>
                      <td colSpan={4}>Paste a shared folder link, then preview its supported PDF or PNG files.</td>
                    </tr>
                  )}
                </tbody>
              </table>
            </div>
          </div>
        </section>
      ) : null}

      {activeTab === "upload" ? (
        <section className="panel grid">
          <div className="stack">
            <label className="upload-dropzone">
              <span className="upload-dropzone-title">Choose PDF or PNG files</span>
              <span className="upload-dropzone-copy">Pick one or more PDFs or PNGs, then keep adding more if needed.</span>
              <span className="upload-dropzone-button">Browse files</span>
              <span className="upload-dropzone-meta">
                {uploadFiles.length ? `${uploadFiles.length} file(s) prepared` : "PDF + PNG"}
              </span>
              <input type="file" accept={uploadAccept} multiple onChange={onFilesSelected} />
            </label>
            <button onClick={handleUploadMerge} disabled={loading || !uploadFiles.length}>
              Merge Uploaded Files
            </button>
          </div>
          <div className="panel inset">
            <h2>Upload review</h2>
            <div className="table-scroll">
              <table>
                <thead>
                  <tr>
                    <th>Order</th>
                    <th>Name</th>
                    <th>Size</th>
                    <th></th>
                  </tr>
                </thead>
                <tbody>
                  {sortedUploadFiles.length ? (
                    sortedUploadFiles.map((item) => (
                      <tr key={item.file.name}>
                        <td>
                          <input
                            className="order-input"
                            type="number"
                            min={1}
                            value={item.order}
                            onChange={(event) => updateUploadOrder(item.file.name, Number(event.target.value))}
                          />
                        </td>
                        <td>{item.file.name}</td>
                        <td>{formatBytes(item.file.size)}</td>
                        <td>
                          <button className="ghost compact" onClick={() => removeUploadFile(item.file.name)}>
                            Remove
                          </button>
                        </td>
                      </tr>
                    ))
                  ) : (
                    <tr>
                      <td colSpan={4}>Upload one or more PDF or PNG files to start a merge job.</td>
                    </tr>
                  )}
                </tbody>
              </table>
            </div>
          </div>
        </section>
      ) : null}

      {activeTab === "history" ? (
        <section className="history-layout">
          <div className="panel">
            <h2>Job history</h2>
            <div className="panel-scroll">
              <ul className="history-list">
                {jobs.length ? (
                  jobs.map((job) => (
                    <li key={job.id}>
                      <button className="history-card" onClick={() => viewJob(job.id)}>
                        <strong>{job.outputFilename}</strong>
                        <span>{job.sourceType} • {job.progressPercent}%</span>
                        <span>{new Date(job.createdAt).toLocaleString()}</span>
                      </button>
                    </li>
                  ))
                ) : (
                  <li className="history-empty">No jobs yet.</li>
                )}
              </ul>
            </div>
          </div>

          <div className="panel">
            <h2>Job detail</h2>
            {selectedJob ? (
              <div className="stack">
                <div className="actions">
                  <button
                    onClick={() => downloadJob(selectedJob.id, selectedJob.outputFilename)}
                    disabled={selectedJob.status !== "completed"}
                  >
                    Download merged PDF
                  </button>
                  <button
                    className="ghost"
                    onClick={() => retryJob(selectedJob.id)}
                    disabled={selectedJob.sourceType !== "drive" || selectedJob.status === "running" || selectedJob.status === "pending"}
                  >
                    Retry
                  </button>
                  <button className="ghost" onClick={() => deleteJob(selectedJob.id)}>Delete job</button>
                </div>
                <div className="detail-meta">
                  <span>Status: {selectedJob.status}</span>
                  <span>Progress: {selectedJob.progressPercent}%</span>
                  {selectedJob.status === "pending" || selectedJob.status === "running" ? (
                    <span>Now: {formatJobProgress(selectedJob, selectedJob.id === activeJob?.id ? downloadSpeedBps : null)}</span>
                  ) : null}
                  {selectedJob.errorMessage ? <span>Error: {selectedJob.errorMessage}</span> : null}
                </div>
                <div className="table-scroll">
                  <table>
                    <thead>
                      <tr>
                        <th>Order</th>
                        <th>Name</th>
                        <th>Source</th>
                        <th>Progress</th>
                      </tr>
                    </thead>
                    <tbody>
                      {selectedJob.files?.map((file, index) => (
                        <tr key={file.id}>
                          <td>{file.order}</td>
                          <td>{file.name}{formatJobFileStatus(selectedJob, file.name, selectedJob.id === activeJob?.id ? downloadSpeedBps : null)}</td>
                          <td>{file.sourceKind}</td>
                          <td>
                            <div className="file-progress-cell">
                              <div className="file-progress-track" aria-hidden="true">
                                <div
                                  className="file-progress-fill"
                                  style={{ width: `${getJobFileProgress(selectedJob, index)}%` }}
                                />
                              </div>
                              <span>{getJobFileProgress(selectedJob, index)}%</span>
                            </div>
                          </td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              </div>
            ) : (
              <p>Select a job to inspect its source files and download the merged output.</p>
            )}
          </div>
        </section>
      ) : null}
    </main>
  );
}

async function api<T>(path: string, options: { method?: string; token?: string; body?: BodyInit | null } = {}): Promise<T> {
  const headers = new Headers();
  if (!(options.body instanceof FormData)) {
    headers.set("Content-Type", "application/json");
  }
  if (options.token) {
    headers.set("Authorization", `Bearer ${options.token}`);
  }

  const response = await fetch(`${apiBaseURL}${path}`, {
    method: options.method ?? "GET",
    headers,
    body: options.body
  });

  if (!response.ok) {
    throw new Error(await readError(response));
  }

  return response.json() as Promise<T>;
}

async function readError(response: Response): Promise<string> {
  try {
    const payload = (await response.json()) as { error?: string };
    return payload.error ?? `Request failed with status ${response.status}`;
  } catch {
    return `Request failed with status ${response.status}`;
  }
}

async function downloadResponse(response: Response, fallbackName: string) {
  const blob = await response.blob();
  const downloadURL = URL.createObjectURL(blob);
  const link = document.createElement("a");
  const contentDisposition = response.headers.get("Content-Disposition");
  const match = contentDisposition?.match(/filename="(.+)"/);
  link.href = downloadURL;
  link.download = match?.[1] ?? fallbackName;
  document.body.appendChild(link);
  link.click();
  link.remove();
  URL.revokeObjectURL(downloadURL);
}

function getErrorMessage(error: unknown) {
  if (error instanceof Error) {
    return error.message;
  }
  return "Unexpected error";
}

function ensureArray<T>(value: T[] | null | undefined): T[] {
  return Array.isArray(value) ? value : [];
}

function isSupportedUploadFile(name: string): boolean {
  const normalized = name.toLowerCase();
  return normalized.endsWith(".pdf") || normalized.endsWith(".png");
}

function formatJobProgress(job: Job, downloadSpeedBps: number | null) {
  if (job.status === "completed") {
    return "Completed.";
  }

  if (job.status === "failed") {
    return job.errorMessage ?? "Merge failed.";
  }

  if (job.currentStage === "downloading") {
    const speedSuffix = formatSpeedSuffix(downloadSpeedBps);
    if (job.totalFiles) {
      return `Downloading ${job.totalFiles} file(s) (${job.progressPercent}%)${speedSuffix}`;
    }
    return `Downloading files (${job.progressPercent}%)${speedSuffix}`;
  }

  if (job.currentStage === "merging") {
    if (job.totalFiles) {
      return `Merging ${job.totalFiles} file(s) (${job.progressPercent}%)`;
    }
    return `Merging files (${job.progressPercent}%)`;
  }

  if (job.currentStage === "uploading") {
    return `Uploading merged PDF (${job.progressPercent}%)`;
  }

  if (job.currentStage === "queued") {
    return `Queued (${job.progressPercent}%)`;
  }

  return `Processing ${job.sourceType} job: ${job.progressPercent}%`;
}

function formatJobFileStatus(job: Job, fileName: string, downloadSpeedBps: number | null) {
  if (job.currentFileName !== fileName) {
    return "";
  }
  const filePercent = getCurrentFilePercent(job);
  const speedSuffix = formatSpeedSuffix(downloadSpeedBps);
  if (filePercent !== null) {
    return ` (downloading ${filePercent}%${speedSuffix})`;
  }
  return " (processing)";
}

function getCurrentFilePercent(job: Job) {
  if (!job.currentFileSize || job.currentFileSize <= 0) {
    return null;
  }
  const percent = Math.floor(((job.currentFileBytes ?? 0) * 100) / job.currentFileSize);
  return Math.max(0, Math.min(100, percent));
}

function getJobFileProgress(job: Job, fileIndex: number) {
  const file = job.files?.[fileIndex];
  if (typeof file?.progressPercent === "number") {
    return file.progressPercent;
  }

  if (job.status === "completed") {
    return 100;
  }

  if (job.sourceType === "upload" && (job.status === "pending" || job.status === "running")) {
    return 100;
  }

  if (job.currentStage === "merging" || job.currentStage === "uploading") {
    return 100;
  }

  if (job.currentStage === "downloading") {
    const activeIndex = Math.max((job.currentFileIndex ?? 1) - 1, 0);
    if (fileIndex < activeIndex) {
      return 100;
    }
    if (fileIndex > activeIndex) {
      return 0;
    }
    return getCurrentFilePercent(job) ?? 0;
  }

  return 0;
}

function getTabFromHash(hash: string): TabKey {
  switch (hash) {
    case "#/upload":
      return "upload";
    case "#/history":
      return "history";
    default:
      return "drive";
  }
}

function tabToHash(tab: TabKey) {
  return `#/${tab}`;
}

function formatSpeed(bytesPerSecond: number | null) {
  if (!bytesPerSecond || bytesPerSecond <= 0) {
    return "";
  }
  return `${formatBytes(bytesPerSecond)}/s`;
}

function formatSpeedSuffix(downloadSpeedBps: number | null) {
  const speed = formatSpeed(downloadSpeedBps);
  return speed ? ` at ${speed}` : "";
}

function formatBytes(bytes: number) {
  if (!bytes) {
    return "-";
  }
  const units = ["B", "KB", "MB", "GB"];
  let size = bytes;
  let unitIndex = 0;
  while (size >= 1024 && unitIndex < units.length - 1) {
    size /= 1024;
    unitIndex += 1;
  }
  return `${size.toFixed(size >= 10 || unitIndex === 0 ? 0 : 1)} ${units[unitIndex]}`;
}

export default App;
