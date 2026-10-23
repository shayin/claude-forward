export const maxUploadFiles = 8;
export const maxUploadBytes = 20 * 1024 * 1024;
const uploadChunkBytes = 256 * 1024;

export function encodeUploadChunk(bytes) {
  if (!(bytes instanceof Uint8Array) || bytes.length === 0 || bytes.length > uploadChunkBytes) {
    throw new Error("上传分片大小无效");
  }
  let binary = "";
  for (let position = 0; position < bytes.length; position += 8192) {
    binary += String.fromCharCode(...bytes.subarray(position, position + 8192));
  }
  return btoa(binary);
}

export async function uploadChunkedFile(file, target, request, stillCurrent, onProgress = () => {}) {
  if (!file || !Number.isInteger(file.size) || file.size < 1 || file.size > maxUploadBytes ||
      typeof file.name !== "string" || file.name === "." || file.name === ".." ||
      file.name.length > 255 || /[/\\\x00-\x1f\x7f]/.test(file.name)) {
    throw new Error("文件名称或大小无效（每个不超过 20 MiB）");
  }
  if (!stillCurrent()) throw new Error("上传目标已切换");
  const common = {
    upload_kind: target.kind,
    workspace_id: target.workspaceID || "",
    surface_id: target.surfaceID || ""
  };
  let uploadId = "";
  try {
    const began = await request("upload_begin", {
      ...common, path: target.path || "", file_name: file.name, size: file.size
    });
    uploadId = began?.upload_id;
    if (typeof uploadId !== "string" || !uploadId) throw new Error("上传未获得会话 ID");
    let offset = 0;
    while (offset < file.size) {
      if (!stillCurrent()) throw new Error("上传目标已切换");
      const bytes = new Uint8Array(await file.slice(offset, offset + uploadChunkBytes).arrayBuffer());
      const response = await request("upload_chunk", {
        ...common, upload_id: uploadId, offset, data: encodeUploadChunk(bytes)
      });
      offset += bytes.length;
      if (response?.written !== offset) throw new Error("上传分片确认不匹配");
      onProgress(offset, file.size);
    }
    if (!stillCurrent()) throw new Error("上传目标已切换");
    const finished = await request("upload_finish", { ...common, upload_id: uploadId });
    if (typeof finished?.path !== "string" || !finished.path.startsWith("/") ||
        finished.size !== file.size) throw new Error("上传结果无效");
    return finished;
  } catch (error) {
    if (uploadId) {
      void request("upload_abort", { ...common, upload_id: uploadId }).catch(() => {});
    }
    throw error;
  }
}
