/** Public format-3 manifest surface; legacy formats remain in release-manifest.ts. */

export {
  PRODUCT_MANIFEST_FORMAT,
  PRODUCT_TOOLCHAIN,
  build_product_manifest,
  validate_product_manifest,
  verify_product_bundle,
  type Artifact,
  type ProductManifest,
  type ProductTarget,
} from "./release-manifest.ts";
