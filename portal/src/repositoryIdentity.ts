import type { RepoRef } from "./api/types";

/**
 * The provider-native path of a configured repository. An Azure DevOps
 * repository lives inside a project, so its identity is
 * `<organization>/<project>/<repository>`; `<owner>/<repository>` alone is not
 * a valid ADO identity (#6799). GitHub and Gitea carry no project and render
 * as `<owner>/<repository>`.
 */
export function repositoryPath(ref: Pick<RepoRef, "owner" | "project" | "name">): string {
  const project = ref.project?.trim();
  return project ? `${ref.owner}/${project}/${ref.name}` : `${ref.owner}/${ref.name}`;
}
