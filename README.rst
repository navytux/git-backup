=======================================================================
 Git-backup - Backup set of Git repositories & just files; efficiently
=======================================================================

:author: Kirill Smelkov <kirr@nexedi.com>
:date:   2015 Aug 31


This program backups files and set of bare Git repositories into one Git repository.
Files are copied to blobs and then added to tree under certain place, and for
Git repositories, all reachable objects are pulled in with maintaining index
which remembers reference -> sha1 for all pulled repositories.

This allows to leverage Git's good data deduplication ability, especially for
cases when there are many hosted repositories which are forks of each other,
and for backup to have history and be otherwise managed as a usual Git
repository.  In particular it is possible to use standard git pull/push to
synchronize backups in several places.

The original motivation for git-backup was to manage backups of `lab.nexedi.com`__
with being able to deduplicate content of forks, and to be able to track the
whole history of the site. The last property is similar to ZODB where Nexedi
used to "never pack" and keep the whole history of the whole site. Please see
the Appendix for more details.

__ https://lab.nexedi.com


Backup workflow is:

1. create backup repository::

     $ mkdir backup
     $ cd backup
     $ git init         # both bare and non-bare possible

2. pull files and Git repositories into backup repository::

     $ git-backup pull dir1:prefix1 dir2:prefix2 ...

   This will pull bare Git repositories & just files from `dir1` into backup
   under `prefix1`, from `dir2` into backup prefix `prefix2`, etc...

3. restore files and Git repositories from backup::

     $ git-backup restore <backup-state-sha1> prefix1:dir1

   Restore Git repositories & just files from backup `prefix1` into `dir1`,
   from backup `prefix2` into `dir2`, etc...

   Backup state to restore is taken from <backup-state-sha1> which is sha1 or
   ref pointing to backup repository state.

4. backup repository itself can be managed with Git. In particular it can be
   synchronized between several places with standard git pull/push, be
   repacked, etc::

     $ git push ...
     $ git pull ...


Please see `git-backup.go`__ source with technical overview on how it works.

We also provide convenience program to pull/restore backup data for a GitLab
instance into/from git-backup managed repository. See `contrib/gitlab-backup`__
for details.


__ git-backup.go
__ contrib/gitlab-backup


--------

Appendix. Original announcement
===============================

:Subject: [Nexedi] [ANNOUNCE] Program to backup several Git repositories into 1
:From: Kirill Smelkov <kirr@nexedi.com>
:Date: Mon, 31 Aug 2015 22:36:31 +0300

Hi All,

Recently we had discussion with Kazuhiko on current GitLab backup state.
GitLab approach is to create tarball for every repository and then
create one big tar file containing everything. In presence of forks this
results in waste of disk space which gets worse the more forks and
personal repositories we have.

Even today, when a lot of development happens not yet on GitLab, 1
standard GitLab backup takes ~ 3GB, which creates pressure for storage
and consequently forces admin to make compromises wrt how long to keep
backup history. Again, this will become more heavy as we move more and
more to GitLab.

So clearly something has to be done.

With this email I propose the idea to backup Git hosting via Git itself.
For this we need to pull all hosted objects (from all git repositories)
into 1 git database and then leverage Git's good ability to deduplicate
and pack content. Plus we need to carefully remember which refs from
which repositories point to which objects so we can properly restore.

That's basically all. I've tried to do a POC which is available here:

    https://lab.nexedi.cn/kirr/git-backup

and contains more details. The main program[1] is generic + there is
concrete driver to backup GitLab repositories together with database
dump and everything else[2].

It has been tested by me on our GitLab instance manually for some time
already and preliminarily results are::

                                    GitLab          POC

    time of 1st run                 2m25s           7m41s
    backup size after 1st run       3013MB          363MB

    time of 2nd run                 1m28s           1m52s
    (with small commit)

    backup size increase            +3013MB         +4MB (*)
    after 2nd run

    (*) I've tracked this +4MB to the fact that git leaves empty directory
        refs/backup/<dir>/ if e.g. refs/backup/<dir>/some-ref was deleted and
        <dir> becomes empty. This can be improved in git itself or worked around
        in the tool. Actual data growth in db objects is few kilobytes.


In other words backup size is already ~10 times smaller compared to
GitLab default and because size increase on incremental runs is small on
average, it creates practical ability to store backup history forever,
just like we do with histories in usual Git repositories.

Restoration process has been also verified manually, and besides that, on
each restore run, the program verifies extracted git repositories for
connectivity correctness. So in my view this should be safe to use.

...

I welcome feedback, questions and review of the tool. If all goes well
and we use it on our GitLab instance for some time ok, my idea is to
make the announcement to a wider audience.

...

| Thanks,
| Kirill

| [1] https://lab.nexedi.cn/kirr/git-backup/blob/master/git-backup
| [2] https://lab.nexedi.cn/kirr/git-backup/blob/master/contrib/gitlab-backup
