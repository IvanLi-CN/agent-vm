/** Public product manifest surface; legacy formats remain in release-manifest.ts. */

export {
  CLIENT_TRANSPORTS,
  LEGACY_PRODUCT_MANIFEST_FORMAT,
  PRODUCT_MANIFEST_FORMAT,
  PRODUCT_LINUX_ARTIFACTS,
  STATE_CAPABILITY_MARKERS,
  PRODUCT_TOOLCHAIN,
  build_product_manifest,
  validate_product_manifest,
  verify_client_sidecar,
  verify_product_bundle,
  type Artifact,
  type ClientTransport,
  type ProductManifest,
  type ProductTarget,
} from "./release-manifest.ts";
