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


Please see `git-backup` source with technical overview on how it works.
